package relay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/perfloop/relay/link"
	"go.uber.org/goleak"
	"golang.org/x/net/http2"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"), goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"))
}

// terminator is the Perfloop end of the tunnel as this relay meets it, in
// the shape package link fixes: a TLS API host whose tunnel route checks the
// bearer token, accepts the WebSocket with the relay subprotocol, and is the
// HTTP/2 client on the socket. Perfloop's own terminator adds what the relay
// never sees (tenant binding, the proxy's signed reads, forwarding between
// replicas) and is tested with the Perfloop API against a relay of this shape.
type terminator struct {
	api *httptest.Server
	cfg Config
	mu  sync.Mutex
	// conns are the tunnels accepted so far, open or ended.
	conns []*http2.ClientConn
}

func newTerminator(t *testing.T, upstreams ...Upstream) *terminator {
	t.Helper()
	term := &terminator{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+link.TunnelPath, term.accept)
	term.api = httptest.NewTLSServer(mux)
	t.Cleanup(func() {
		term.mu.Lock()
		conns := term.conns
		term.mu.Unlock()
		for _, cc := range conns {
			_ = cc.Close()
		}
		term.api.Close()
	})
	// The relay trusts exactly the certificate the test API presents.
	caPath := filepath.Join(t.TempDir(), "api-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: term.api.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	term.cfg = Config{API: term.api.URL, APICA: caPath, Token: "tenant-seven", Upstreams: upstreams}
	return term
}

// accept is the tunnel route: one token is known, the subprotocol is
// required, and the socket becomes an HTTP/2 client connection.
func (m *terminator) accept(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Authorization") != "Bearer tenant-seven" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ws, err := websocket.Accept(w, req, &websocket.AcceptOptions{Subprotocols: []string{link.Subprotocol}})
	if err != nil {
		return
	}
	if ws.Subprotocol() != link.Subprotocol {
		_ = ws.Close(websocket.StatusPolicyViolation, "perfloop-relay subprotocol is required")
		return
	}
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	cc, err := (&http2.Transport{AllowHTTP: true, DisableCompression: true}).NewClientConn(conn)
	if err != nil {
		_ = ws.CloseNow()
		return
	}
	m.mu.Lock()
	m.conns = append(m.conns, cc)
	m.mu.Unlock()
}

// live returns the tunnels that are open.
func (m *terminator) live() []*http2.ClientConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	var live []*http2.ClientConn
	for _, cc := range m.conns {
		if !cc.State().Closed {
			live = append(live, cc)
		}
	}
	return live
}

func (m *terminator) waitTunnels(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(m.live()) != want {
		if time.Now().After(deadline) {
			t.Fatalf("tunnels=%d, want %d", len(m.live()), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// replace ends every open tunnel as an API rollout does: GOAWAY, in-flight
// reads finish, the socket closes. The relay reopens them against the same
// host.
func (m *terminator) replace(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for _, cc := range m.live() {
		_ = cc.Shutdown(ctx)
		_ = cc.Close()
	}
}

// run starts a relay against the terminator and returns its stop function.
// The relay stops before the servers close.
func (m *terminator) run(t *testing.T, proxy ...func(*http.Request) (*url.URL, error)) context.CancelFunc {
	t.Helper()
	r, err := New(m.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if len(proxy) > 0 {
		r.client.Transport.(*http.Transport).Proxy = proxy[0]
	}
	return m.start(t, r)
}

func (m *terminator) start(t *testing.T, r *Relay) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel
}

// reply is what a test needs from a closed read response.
type reply struct {
	status int
	header http.Header
	body   string
}

// read sends one read down a live tunnel, as Perfloop does: the upstream
// name as the host, the request line unchanged, and returns the closed
// response.
func (m *terminator) read(t *testing.T, upstream, path string, headers ...string) reply {
	t.Helper()
	resp := m.readRaw(t, http.MethodGet, upstream, path, http.NoBody, headers...)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

// readRaw is read with the body still open, for tests that watch the stream.
func (m *terminator) readRaw(t *testing.T, method, upstream, path string, body io.Reader, headers ...string) *http.Response {
	t.Helper()
	live := m.live()
	if len(live) == 0 {
		t.Fatal("no live tunnel to read through")
	}
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+upstream+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = upstream
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := live[0].RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestReadsReachOnlyDocumentedRoutesWithLocalCredentials(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		calls = append(calls, req.URL.RequestURI()+" auth="+req.Header.Get("Authorization")+" accept="+req.Header.Get("Accept"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"success"}`)
	}))
	defer provider.Close()
	term := newTerminator(t,
		Upstream{Name: "vm", Kind: "victoriametrics", URL: provider.URL + "/select/0/prometheus", Headers: map[string]string{"Authorization": "Bearer local-token"}},
		Upstream{Name: "logs", Kind: "loki", URL: provider.URL})
	term.run(t)
	term.waitTunnels(t, tunnels)

	resp := term.read(t, "vm", "/api/v1/query?query=up", "Accept", "application/json", "Authorization", "Bearer from-perfloop")
	if resp.status != http.StatusOK || resp.body != `{"status":"success"}` || resp.header.Get("Content-Type") != "application/json" {
		t.Fatalf("query: %+v", resp)
	}
	if want := "/select/0/prometheus/api/v1/query?query=up auth=Bearer local-token accept=application/json"; len(calls) != 1 || calls[0] != want {
		t.Fatalf("provider saw %q, want %q", calls, want)
	}
	if labels := term.read(t, "logs", "/loki/api/v1/labels?start=1&end=2"); labels.status != http.StatusOK {
		t.Fatalf("loki labels: %+v", labels)
	}
	for _, refused := range []struct {
		upstream, path string
		status         int
	}{
		{"vm", "/api/v1/admin/tsdb/delete_series?match[]=up", http.StatusForbidden},
		{"vm", "/api/v1/admin/tsdb/snapshot/api/v1/query", http.StatusForbidden},
		{"vm", "/api/v1/write", http.StatusForbidden},
		{"vm", "/select/0/prometheus/api/v1/../v1/query", http.StatusBadRequest},
		{"logs", "/loki/api/v1/series", http.StatusForbidden},
		{"logs", "/loki/api/v1/push", http.StatusForbidden},
		{"other", "/api/v1/query", http.StatusNotFound},
	} {
		if resp := term.read(t, refused.upstream, refused.path); resp.status != refused.status {
			t.Fatalf("%s %s: %d, want %d from the relay", refused.upstream, refused.path, resp.status, refused.status)
		}
	}
	post := term.readRaw(t, http.MethodPost, "vm", "/api/v1/query", strings.NewReader("query=up"))
	post.Body.Close()
	if post.StatusCode != http.StatusMethodNotAllowed || len(calls) != 2 {
		t.Fatalf("POST answered %s; provider calls %d, want the two reads only", post.Status, len(calls))
	}
}

// A stopped relay leaves no tunnel; a restarted one serves again.
func TestStoppedRelayLeavesNoTunnelUntilItReturns(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	defer provider.Close()
	term := newTerminator(t, Upstream{Name: "vm", Kind: "prometheus", URL: provider.URL})
	stop := term.run(t)
	term.waitTunnels(t, tunnels)
	stop()
	term.waitTunnels(t, 0)
	term.run(t)
	term.waitTunnels(t, tunnels)
	if resp := term.read(t, "vm", "/api/v1/query"); resp.status != http.StatusOK || resp.body != "ok" {
		t.Fatalf("read after the relay returned: %+v", resp)
	}
}

// Perfloop closing every tunnel (a rollout, the balancer's daily cut) makes
// the relay reopen them, and reads flow again.
func TestRelayReopensTunnelsPerfloopClosed(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	defer provider.Close()
	term := newTerminator(t, Upstream{Name: "vm", Kind: "prometheus", URL: provider.URL})
	term.run(t)
	term.waitTunnels(t, tunnels)
	term.replace(t)
	term.waitTunnels(t, tunnels)
	if resp := term.read(t, "vm", "/api/v1/query"); resp.status != http.StatusOK {
		t.Fatalf("read after Perfloop rolled its replica: %+v", resp)
	}
}

func TestOversizedResponseIsCutNotForwarded(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := strings.Repeat("x", 1<<20)
		for range maxResponseBytes/len(chunk) + 2 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	defer provider.Close()
	term := newTerminator(t, Upstream{Name: "vm", Kind: "prometheus", URL: provider.URL})
	term.run(t)
	term.waitTunnels(t, tunnels)
	resp := term.readRaw(t, http.MethodGet, "vm", "/api/v1/query", http.NoBody)
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err == nil || n > maxResponseBytes+1 {
		t.Fatalf("oversized response copied %d bytes with error %v; want a broken read", n, err)
	}
}

func TestTunnelHonorsHTTPSProxyAndUpstreamsDoNot(t *testing.T) {
	var connects atomic.Int64
	egress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect {
			http.Error(w, "connect only", http.StatusMethodNotAllowed)
			return
		}
		upstream, err := (&net.Dialer{}).DialContext(req.Context(), "tcp", req.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		connects.Add(1)
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { defer upstream.Close(); io.Copy(upstream, rw.Reader) }()
		io.Copy(conn, upstream)
		conn.Close()
	}))
	defer egress.Close()
	// http.ProxyFromEnvironment never proxies loopback targets, so the test
	// sets the transport's proxy directly; production reads HTTPS_PROXY.
	egressURL, _ := url.Parse(egress.URL)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	defer provider.Close()
	term := newTerminator(t, Upstream{Name: "vm", Kind: "prometheus", URL: provider.URL})
	term.run(t, func(*http.Request) (*url.URL, error) { return egressURL, nil })
	term.waitTunnels(t, tunnels)
	if connects.Load() != int64(tunnels) {
		t.Fatalf("egress proxy saw %d CONNECTs, want %d", connects.Load(), tunnels)
	}
	// The egress proxy accepts only CONNECT, so a plain provider read routed
	// through it would fail; a successful read proves upstreams bypass it.
	if resp := term.read(t, "vm", "/api/v1/query"); resp.status != http.StatusOK || connects.Load() != int64(tunnels) {
		t.Fatalf("read: %d, egress CONNECTs %d", resp.status, connects.Load())
	}
}

func TestTunnelRefusesUntrustedAPICertificate(t *testing.T) {
	term := newTerminator(t, Upstream{Name: "vm", Kind: "prometheus", URL: "http://vm.internal:8428"})
	// Trust an unrelated self-signed certificate for the right address.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, IsCA: true, BasicConstraintsValid: true}
	other, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(term.cfg.APICA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: other}), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := New(term.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.open(t.Context()); err == nil || !strings.Contains(err.Error(), "tls:") {
		t.Fatalf("untrusted API accepted: %v", err)
	}
	if len(term.live()) != 0 {
		t.Fatal("tunnel registered without trusted TLS")
	}
}

// A bad token gets a clear refusal and no tunnel.
func TestTunnelReportsRefusedToken(t *testing.T) {
	term := newTerminator(t, Upstream{Name: "vm", Kind: "prometheus", URL: "http://vm.internal:8428"})
	term.cfg.Token = "tenant-eight"
	r, err := New(term.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.open(t.Context()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("refused token: %v", err)
	}
	if len(term.live()) != 0 {
		t.Fatal("tunnel registered with a refused token")
	}
}

func TestTunnelSendsTheTokenOnlyToTheConfiguredAPI(t *testing.T) {
	// The API host answers the tunnel route with a redirect on its own host.
	// The token must not follow it: the handshake fails and the redirect's
	// target sees no request.
	var followed atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+link.TunnelPath, func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/elsewhere"+link.TunnelPath, http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/elsewhere/", func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
		http.Error(w, "moved", http.StatusUnauthorized)
	})
	api := httptest.NewTLSServer(mux)
	defer api.Close()
	caPath := filepath.Join(t.TempDir(), "api-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{API: api.URL, APICA: caPath, Token: "tenant-seven", Upstreams: []Upstream{{Name: "vm", Kind: "prometheus", URL: "http://vm.internal:8428"}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.open(t.Context()); err == nil || followed.Load() != 0 {
		t.Fatalf("redirected handshake: error=%v followed=%d", err, followed.Load())
	}
}

// The read id Perfloop gave a read is in the relay's audit line and not in
// the upstream request: the customer joins their log to the Perfloop
// transcript, and their provider never sees a Perfloop identifier.
func TestReadIDIsLoggedAndNotForwardedUpstream(t *testing.T) {
	var mu sync.Mutex
	var upstreamSaw []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		upstreamSaw = append(upstreamSaw, req.Header.Get(link.RequestIDHeader))
		mu.Unlock()
		io.WriteString(w, "ok")
	}))
	defer provider.Close()
	term := newTerminator(t, Upstream{Name: "vm", Kind: "prometheus", URL: provider.URL})
	var log bytes.Buffer
	var logMu sync.Mutex
	r, err := New(term.cfg, slog.New(slog.NewJSONHandler(&lockedWriter{mu: &logMu, w: &log}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	term.start(t, r)
	term.waitTunnels(t, tunnels)
	const id = "ses-1/call_3"
	resp := term.read(t, "vm", "/api/v1/query?query=up", link.RequestIDHeader, id)
	if resp.status != http.StatusOK || len(upstreamSaw) != 1 || upstreamSaw[0] != "" {
		t.Fatalf("status=%d upstream saw read id %q; want none", resp.status, upstreamSaw)
	}
	logMu.Lock()
	lines := log.String()
	logMu.Unlock()
	var found bool
	for line := range strings.SplitSeq(strings.TrimSpace(lines), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil || entry["msg"] != "relay read" {
			continue
		}
		found = true
		if entry["request_id"] != id || entry["decision"] != "query" || entry["upstream"] != "vm" {
			t.Fatalf("relay read line %s; want request_id %q", line, id)
		}
	}
	if !found {
		t.Fatalf("no relay read line in %s", lines)
	}
}

// lockedWriter serializes the relay's log lines with the test's read of them.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
