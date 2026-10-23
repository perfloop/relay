package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/perfloop/relay/link"
)

// openCodehost waits for successful CONNECT headers before sending any body
// bytes. A relay that waits for ClientHello before flushing cannot pass it.
func openCodehost(t *testing.T, ctx context.Context, term *terminator) (*http.Response, *io.PipeWriter) {
	t.Helper()
	reader, writer := io.Pipe()
	// The HTTP/2 client can be blocked reading an idle upload even after
	// RoundTrip returns headers. The client must close that pipe on cancel
	// so its write loop can send RST_STREAM to the relay.
	stop := context.AfterFunc(ctx, func() { _ = reader.CloseWithError(ctx.Err()) })
	t.Cleanup(func() { stop(); _ = reader.Close(); _ = writer.Close() })
	req, err := http.NewRequestWithContext(ctx, http.MethodConnect, "https://git", reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(link.RequestIDHeader, "ses-1/codehost")
	req.Header.Set("X-Outside-TLS", "not-forwarded")
	resp, err := term.live()[0].RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || resp.Header.Get(link.ErrorHeader) != "" {
		t.Fatalf("CONNECT: status=%s headers=%v", resp.Status, resp.Header)
	}
	return resp, writer
}

// connectSocket gives the test's TLS client a net.Conn on an HTTP/2 CONNECT
// body pair. The pipes bridge bytes only; the TLS client owns trust and auth.
func connectSocket(t *testing.T, resp *http.Response, writer *io.PipeWriter) net.Conn {
	t.Helper()
	client, stream := net.Pipe()
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = io.Copy(writer, stream); _ = writer.Close() })
	wg.Go(func() { _, _ = io.Copy(stream, resp.Body); _ = stream.Close() })
	t.Cleanup(func() {
		_ = client.Close()
		_ = stream.Close()
		_ = writer.Close()
		_ = resp.Body.Close()
		wg.Wait()
	})
	return client
}

// tcpCodehost accepts raw TCP, not TLS, so tests can prove that CONNECT
// carries opaque bytes. Deadlines and cleanup bound fixture lifetime on failure.
func tcpCodehost(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{})
	var wg sync.WaitGroup
	go func() {
		defer close(accepted)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
				handler(conn)
			})
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-accepted; wg.Wait() })
	return "https://" + listener.Addr().String()
}

// TLS terminates at the provider, not the relay. Only the client trusts the
// certificate and supplies provider auth. Reachability alone still gets 401.
func TestCodehostTLSAndAuthorizationStayEndToEnd(t *testing.T) {
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.TLS == nil || req.TLS.ServerName != "example.com" || req.URL.Path != "/repository" {
			t.Errorf("code host received TLS=%+v path=%s", req.TLS, req.URL.Path)
		}
		if req.Header.Get("X-Outside-TLS") != "" || req.Header.Get(link.RequestIDHeader) != "" {
			t.Error("CONNECT headers reached the code host")
		}
		if req.Header.Get("Authorization") != "Bearer proxy-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Perfloop-Provider-Value", "opaque")
		_, _ = io.WriteString(w, "repository bytes")
	}))
	defer provider.Close()
	term := newTerminator(t, Upstream{Name: "git", Kind: "codehost", URL: provider.URL})
	term.run(t)
	term.waitTunnels(t, tunnels)
	roots := x509.NewCertPool()
	roots.AddCert(provider.Certificate())
	for _, tc := range []struct {
		name, auth string
		status     int
	}{
		{"authorized", "Bearer proxy-token", http.StatusOK},
		{"transport alone", "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			resp, writer := openCodehost(t, ctx, term)
			defer resp.Body.Close()
			conn := tls.Client(connectSocket(t, resp, writer), &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12})
			defer conn.Close()
			if err := conn.HandshakeContext(ctx); err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/repository", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			req.Close = true
			req.Header.Set("Authorization", tc.auth)
			if err := req.Write(conn); err != nil {
				t.Fatal(err)
			}
			inner, err := http.ReadResponse(bufio.NewReader(conn), req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(inner.Body)
			_ = inner.Body.Close()
			if err != nil || inner.StatusCode != tc.status {
				t.Fatalf("provider: status=%s body=%q error=%v", inner.Status, body, err)
			}
			if tc.status == http.StatusOK && (string(body) != "repository bytes" || inner.Header.Get("Perfloop-Provider-Value") != "opaque") {
				t.Fatalf("encrypted response was changed: %v %q", inner.Header, body)
			}
			if resp.Header.Get("Perfloop-Provider-Value") != "" {
				t.Fatal("provider headers appeared outside TLS")
			}
		})
	}
	for _, tc := range []struct {
		name, serverName string
		roots            *x509.CertPool
	}{
		{"untrusted certificate", "example.com", x509.NewCertPool()},
		{"wrong server name", "wrong.example.net", roots},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			resp, writer := openCodehost(t, ctx, term)
			defer resp.Body.Close()
			conn := tls.Client(connectSocket(t, resp, writer), &tls.Config{RootCAs: tc.roots, ServerName: tc.serverName, MinVersion: tls.VersionTLS12})
			defer conn.Close()
			if err := conn.HandshakeContext(ctx); err == nil {
				t.Fatal("client accepted invalid code-host TLS")
			}
		})
	}
}

// A CONNECT carries more than the telemetry limit in both directions, with
// exact bytes and a clean EOF. END_STREAM half-closes TCP, not its response.
func TestCodehostOpaqueStreamExceedsTelemetryLimit(t *testing.T) {
	origin := tcpCodehost(t, func(conn net.Conn) { _, _ = io.Copy(conn, conn) })
	term := newTerminator(t, Upstream{Name: "git", Kind: "codehost", URL: origin})
	term.run(t)
	term.waitTunnels(t, tunnels)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	resp, writer := openCodehost(t, ctx, term)
	defer resp.Body.Close()
	payload := bytes.Repeat([]byte("opaque\x00\xff"), maxResponseBytes/8+1024)
	written := make(chan error, 1)
	go func() { _, err := writer.Write(payload); _ = writer.Close(); written <- err }()
	hash := sha256.New()
	n, err := io.Copy(hash, resp.Body)
	want := sha256.Sum256(payload)
	if err != nil || n != int64(len(payload)) || !bytes.Equal(hash.Sum(nil), want[:]) {
		t.Fatalf("opaque stream: bytes=%d want=%d error=%v", n, len(payload), err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}

// An idle connection ends with its request or tunnel. No extra lifetime or
// retry loop may keep a code-host socket alive after the transport ends.
func TestCodehostConnectionFollowsStreamAndTunnelLifetime(t *testing.T) {
	for _, action := range []string{"request canceled", "response closed", "Run stopped", "tunnel closed"} {
		t.Run(action, func(t *testing.T) {
			closed := make(chan error, 1)
			origin := tcpCodehost(t, func(conn net.Conn) {
				_, _ = io.WriteString(conn, "ready")
				_, err := io.Copy(io.Discard, conn)
				closed <- err
			})
			term := newTerminator(t, Upstream{Name: "git", Kind: "codehost", URL: origin})
			stop := term.run(t)
			term.waitTunnels(t, tunnels)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			resp, _ := openCodehost(t, ctx, term)
			defer resp.Body.Close()
			ready := make([]byte, len("ready"))
			if _, err := io.ReadFull(resp.Body, ready); err != nil || string(ready) != "ready" {
				t.Fatalf("upstream greeting: %q %v", ready, err)
			}
			switch action {
			case "request canceled":
				cancel()
			case "response closed":
				_ = resp.Body.Close()
			case "Run stopped":
				stop()
			case "tunnel closed":
				_ = term.live()[0].Close()
			}
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal("upstream did not receive EOF:", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("code-host connection survived its transport")
			}
		})
	}
}

// The fixed name is not an arbitrary TCP destination, and a codehost cannot
// use the telemetry handler. All refusals occur before the dial.
func TestCodehostRefusesOtherNamesMethodsAndPaths(t *testing.T) {
	r, err := New(Config{API: "https://app.perfloop.ai", Token: "t", Upstreams: []Upstream{
		{Name: "git", Kind: "codehost", URL: "https://git.example.com"},
		{Name: "vm", Kind: "prometheus", URL: "http://vm.example.com"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host, method, path, query string
		proto, status             int
	}{
		{"git", http.MethodGet, "/api/v1/query", "", 2, http.StatusMethodNotAllowed},
		{"git", http.MethodPost, "", "", 2, http.StatusMethodNotAllowed},
		{"git", http.MethodConnect, "", "", 1, http.StatusMethodNotAllowed},
		{"git", http.MethodConnect, "/", "", 2, http.StatusBadRequest},
		{"git", http.MethodConnect, "/repository", "", 2, http.StatusBadRequest},
		{"git", http.MethodConnect, "", "destination=elsewhere:443", 2, http.StatusBadRequest},
		{"git:443", http.MethodConnect, "", "", 2, http.StatusNotFound},
		{"git.example.com", http.MethodConnect, "", "", 2, http.StatusNotFound},
		{"other", http.MethodConnect, "", "", 2, http.StatusNotFound},
		{"GIT", http.MethodConnect, "", "", 2, http.StatusNotFound},
		{"vm", http.MethodConnect, "", "", 2, http.StatusMethodNotAllowed},
	} {
		req := &http.Request{Method: tc.method, Host: tc.host, ProtoMajor: tc.proto, URL: &url.URL{Path: tc.path, RawQuery: tc.query}, Header: make(http.Header), Body: http.NoBody}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.status || w.Header().Get(link.ErrorHeader) != link.Refused {
			t.Errorf("%s %s %q?%s HTTP/%d: status=%d headers=%v", tc.method, tc.host, tc.path, tc.query, tc.proto, w.Code, w.Header())
		}
	}
}

// A reset after some bytes produces a broken stream, never a replay or an
// apparently complete short response. The relay keeps the other tunnel live.
func TestCodehostResetBreaksStreamWithoutReplay(t *testing.T) {
	release := make(chan struct{})
	releaseAll := sync.OnceFunc(func() { close(release) })
	defer releaseAll()
	var calls atomic.Int64
	origin := tcpCodehost(t, func(conn net.Conn) {
		calls.Add(1)
		_, _ = io.WriteString(conn, "prefix")
		<-release
		_ = conn.(*net.TCPConn).SetLinger(0)
	})
	term := newTerminator(t, Upstream{Name: "git", Kind: "codehost", URL: origin})
	term.run(t)
	term.waitTunnels(t, tunnels)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resp, _ := openCodehost(t, ctx, term)
	defer resp.Body.Close()
	prefix := make([]byte, len("prefix"))
	if _, err := io.ReadFull(resp.Body, prefix); err != nil || string(prefix) != "prefix" {
		t.Fatalf("prefix: %q %v", prefix, err)
	}
	releaseAll()
	if _, err := io.Copy(io.Discard, resp.Body); err == nil {
		t.Fatal("reset stream ended with a clean EOF")
	}
	if calls.Load() != 1 || len(term.live()) != tunnels {
		t.Fatalf("connections=%d tunnels=%d; want one attempt and %d live tunnels", calls.Load(), len(term.live()), tunnels)
	}
}

func TestCodehostDialFailureIsMarkedBeforeSuccess(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "https://" + listener.Addr().String()
	_ = listener.Close()
	term := newTerminator(t, Upstream{Name: "git", Kind: "codehost", URL: origin})
	term.run(t)
	term.waitTunnels(t, tunnels)
	resp := term.readRaw(t, http.MethodConnect, "git", "", http.NoBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get(link.ErrorHeader) != link.Refused {
		t.Fatalf("dial failure: status=%s headers=%v", resp.Status, resp.Header)
	}
}

// CONNECTs use the existing HTTP/2 stream slots, not a second concurrency
// allowance. With eight open, even a telemetry GET on that tunnel must wait.
func TestCodehostSharesTheTunnelStreamLimitWithTelemetry(t *testing.T) {
	origin := tcpCodehost(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	var reads atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer provider.Close()
	term := newTerminator(t, Upstream{Name: "git", Kind: "codehost", URL: origin}, Upstream{Name: "vm", Kind: "prometheus", URL: provider.URL})
	term.run(t)
	term.waitTunnels(t, tunnels)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range maxStreams {
		resp, _ := openCodehost(t, ctx, term)
		defer resp.Body.Close()
	}
	wait, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	req, err := http.NewRequestWithContext(wait, http.MethodGet, "http://vm/api/v1/query", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := term.live()[0].RoundTrip(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a ninth stream passed while eight CONNECTs were open")
	}
	if reads.Load() != 0 || term.live()[0].State().StreamsActive != maxStreams {
		t.Fatalf("GETs=%d tunnel=%+v; want no GET and eight active streams", reads.Load(), term.live()[0].State())
	}
}
