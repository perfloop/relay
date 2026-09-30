// Package relay is the customer end of a Perfloop relay tunnel. It runs
// inside the customer network, opens WebSockets to the Perfloop API, and
// answers the proxy's validated telemetry reads by forwarding them to the
// configured upstreams. It collects nothing: no telemetry is stored or pushed,
// no inbound port is opened, and provider URLs and credentials stay here.
// README.md describes the contract; package link owns the wire shape.
package relay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/perfloop/relay/link"
	"golang.org/x/net/http2"
)

const (
	// tunnels is how many tunnels the relay keeps open. Two, so the recycling
	// or loss of one never leaves the tenant without a tunnel.
	tunnels = 2
	// maxStreams bounds the reads in flight on one tunnel; the tunnel's
	// HTTP/2 settings enforce it, so Perfloop cannot exceed it.
	maxStreams = 8
	// maxResponseBytes bounds one upstream response. The proxy's own limits
	// are far smaller; this stops a runaway provider from filling the tunnel.
	maxResponseBytes = 32 << 20
	// readTimeout bounds one upstream read end to end.
	readTimeout = 2 * time.Minute
	// dialTimeout bounds each step of opening a tunnel: dial, TLS, and the
	// WebSocket handshake.
	dialTimeout = 15 * time.Second
	// readIdle is how long a tunnel may be silent before the relay pings
	// Perfloop; pingTimeout ends a tunnel whose ping gets no answer. Perfloop
	// pings on the same schedule, so a dead peer is noticed within their sum
	// from either side.
	readIdle    = 30 * time.Second
	pingTimeout = 10 * time.Second
	// maxBackoff caps the wait between reconnects. Tunnels are cheap, and this
	// is how long reads stay unavailable after Perfloop comes back.
	maxBackoff = 10 * time.Second
	// maxFrame bounds one WebSocket message; HTTP/2 frames are far smaller.
	maxFrame = 1 << 20
)

// Relay answers proxy reads from configured upstreams over tunnels to
// Perfloop.
type Relay struct {
	cfg       Config
	api       *url.URL
	upstreams map[string]*upstream
	logger    *slog.Logger
	// client reaches Perfloop and nothing else: it honors HTTPS_PROXY, trusts
	// the system roots or only the configured CA, and speaks HTTP/1.1 so the
	// WebSocket handshake can upgrade.
	client *http.Client
}

type upstream struct {
	Upstream
	base   *url.URL
	client *http.Client
}

// New validates the configuration and builds a relay.
func New(cfg Config, logger *slog.Logger) (*Relay, error) {
	err := cfg.Validate()
	if err != nil {
		return nil, fmt.Errorf("relay config: %w", err)
	}
	api, _ := cfg.apiURL()
	var roots *x509.CertPool
	if cfg.APICA != "" {
		if roots, err = loadRoots(cfg.APICA); err != nil {
			return nil, fmt.Errorf("api_ca: %w", err)
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: dialTimeout,
		Protocols:           new(http.Protocols),
	}
	transport.Protocols.SetHTTP1(true)
	// The token goes to the configured API origin and nowhere else: a
	// redirect, to any host, ends the handshake instead of being followed.
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r := &Relay{cfg: cfg, api: api, upstreams: make(map[string]*upstream, len(cfg.Upstreams)), logger: logger, client: client}
	for _, up := range cfg.Upstreams {
		base, _ := url.Parse(up.URL)
		var tlsRoots *x509.CertPool
		if up.CA != "" {
			if tlsRoots, err = loadRoots(up.CA); err != nil {
				return nil, fmt.Errorf("upstream %s ca: %w", up.Name, err)
			}
		}
		// Proxy is nil on purpose: upstreams are local, so HTTPS_PROXY (which
		// the tunnel honors) must not apply to them. DisableCompression keeps
		// provider bytes exactly as the proxy asked for them.
		upstreamTransport := &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
			TLSClientConfig:       &tls.Config{RootCAs: tlsRoots, MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2:     true,
			DisableCompression:    true,
			MaxIdleConnsPerHost:   maxStreams,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: readTimeout,
		}
		r.upstreams[up.Name] = &upstream{
			Upstream: up,
			base:     base,
			client: &http.Client{Transport: upstreamTransport, Timeout: readTimeout,
				CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("upstream redirects are not followed") }},
		}
	}
	return r, nil
}

func loadRoots(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("no certificate in PEM file")
	}
	return roots, nil
}

// Run keeps the tunnels open until ctx ends. A tunnel that ends, for any
// reason, is reopened with backoff; a tunnel that lasted a minute resets the
// backoff. When ctx ends the tunnels close at once; Perfloop moves a read
// whose tunnel failed before its response started to another tunnel.
func (r *Relay) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := range tunnels {
		wg.Go(func() { r.keep(ctx, i) })
	}
	wg.Wait()
	return ctx.Err()
}

func (r *Relay) keep(ctx context.Context, n int) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := r.serve(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		r.logger.Warn("tunnel ended", "tunnel", n, "error", err, "retry", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// open performs the WebSocket handshake with Perfloop and returns the tunnel
// as a connection. net/http owns the dial: HTTPS_PROXY and its CONNECT, the
// configured trust, and the handshake timeouts all come from the client.
func (r *Relay) open(ctx context.Context) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	target := *r.api
	target.Scheme = "wss"
	target.Path = link.TunnelPath
	header := http.Header{"Authorization": {"Bearer " + r.cfg.Token}}
	ws, resp, err := websocket.Dial(ctx, target.String(), &websocket.DialOptions{HTTPClient: r.client, HTTPHeader: header, Subprotocols: []string{link.Subprotocol}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("perfloop refused tunnel: %s", resp.Status)
		}
		return nil, fmt.Errorf("tunnel: %w", err)
	}
	ws.SetReadLimit(maxFrame)
	return websocket.NetConn(context.Background(), ws, websocket.MessageBinary), nil
}

// serve opens one tunnel and answers reads on it until it ends. Inside the
// tunnel the relay is the HTTP/2 server: Perfloop opens streams, the relay
// never does.
func (r *Relay) serve(ctx context.Context) error {
	conn, err := r.open(ctx)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	r.logger.Info("tunnel open", "api", r.api.Host)
	server := &http2.Server{MaxConcurrentStreams: maxStreams, ReadIdleTimeout: readIdle, PingTimeout: pingTimeout}
	server.ServeConn(conn, &http2.ServeConnOpts{Context: ctx, Handler: r})
	return errors.New("perfloop closed the tunnel")
}

// ServeHTTP answers one proxy read from the tunnel. The Host names the
// upstream; the path is forwarded under the upstream URL when its route is a
// documented read. One audit line records every decision with the read id
// Perfloop gave it, `<session id>/<tool call ref>`, which names the
// transcript turn that asked; a read no tool call made has none.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	status, decision, err := r.forward(w, req)
	r.logger.Info("relay read", "upstream", req.Host, "method", req.Method, "path", req.URL.Path, "request_id", req.Header.Get(link.RequestIDHeader),
		"status", status, "decision", decision, "ms", time.Since(start).Milliseconds(), "error", err)
	if decision == "truncated" {
		// The upstream status is already sent. Abort so Perfloop and the
		// proxy see a broken read, never a short body that looks complete.
		panic(http.ErrAbortHandler)
	}
}

func (r *Relay) forward(w http.ResponseWriter, req *http.Request) (int, string, error) {
	up := r.upstreams[req.Host]
	if up == nil {
		return refuse(w, http.StatusNotFound, "unknown upstream"), "unknown-upstream", nil
	}
	if req.Method != http.MethodGet || req.ContentLength != 0 {
		return refuse(w, http.StatusMethodNotAllowed, "only GET without a body is relayed"), "method", nil
	}
	if p := req.URL.Path; p == "" || p[0] != '/' || p != path.Clean(p) {
		return refuse(w, http.StatusBadRequest, "path is not clean"), "path", nil
	}
	name, ok := route(up.Kind, req.URL.Path)
	if !ok {
		return refuse(w, http.StatusForbidden, "route is not a documented read"), "route", nil
	}
	// One read is bounded end to end on both sides: the upstream client's
	// timeout, and the same deadline on the response write, so a tunnel peer
	// that withholds flow-control credit cannot hold the stream forever.
	ctx, cancel := context.WithTimeout(req.Context(), readTimeout)
	defer cancel()
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(readTimeout)); err != nil {
		return refuse(w, http.StatusInternalServerError, "write deadline is not supported"), "path", err
	}
	target := *up.base
	target.Path = path.Join(up.base.Path, req.URL.Path)
	target.RawQuery = req.URL.RawQuery
	out, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return refuse(w, http.StatusBadRequest, "request could not be built"), "path", err
	}
	// Only the headers Perfloop is known to send cross to the upstream; the
	// read id and anything else stop here. The customer's headers come last
	// and win.
	for _, name := range link.ForwardedHeaders {
		if values := req.Header.Values(name); len(values) > 0 {
			out.Header[http.CanonicalHeaderKey(name)] = slices.Clone(values)
		}
	}
	for header, value := range up.Headers {
		out.Header.Set(header, value)
	}
	resp, err := up.client.Do(out)
	if err != nil {
		// The client's error names the full URL, query included; the audit
		// line carries the path only, so log the cause without it.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return refuse(w, http.StatusBadGateway, "upstream request failed"), "upstream-error", err
	}
	defer resp.Body.Close()
	maps.Copy(w.Header(), resp.Header)
	link.StripHop(w.Header())
	// An upstream cannot speak as Perfloop: no Perfloop-* header of its
	// making reaches the tunnel, whatever the API strips on its side.
	for name := range w.Header() {
		if strings.HasPrefix(name, "Perfloop-") {
			w.Header().Del(name)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return resp.StatusCode, "truncated", err
	}
	if n > maxResponseBytes {
		return resp.StatusCode, "truncated", fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	return resp.StatusCode, name, nil
}

func refuse(w http.ResponseWriter, status int, reason string) int {
	http.Error(w, reason, status)
	return status
}
