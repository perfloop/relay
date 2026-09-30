package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/perfloop/relay/link"
)

// Only the headers Perfloop is known to send reach the upstream; a hostile
// extra header never does, and the customer's configured header wins over
// the one Perfloop sent.
func TestOnlyAllowlistedRequestHeadersReachTheUpstream(t *testing.T) {
	var mu sync.Mutex
	var seen http.Header
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		seen = req.Header.Clone()
		mu.Unlock()
		io.WriteString(w, "ok")
	}))
	defer provider.Close()
	term := newTerminator(t, Upstream{Name: "logs", Kind: "loki", URL: provider.URL, Headers: map[string]string{"X-Scope-OrgID": "team-a"}})
	term.run(t)
	term.waitTunnels(t, tunnels)
	resp := term.read(t, "logs", "/loki/api/v1/labels",
		"Accept", "application/json", "Accept-Encoding", "gzip", "User-Agent", "perfloop", "X-Scope-OrgID", "from-perfloop",
		"X-Forwarded-Host", "evil", "Forwarded", "for=evil", "Cookie", "a=b", "Authorization", "Bearer from-perfloop", link.RequestIDHeader, "ses/call")
	if resp.status != http.StatusOK {
		t.Fatalf("read: %+v", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	for name, want := range map[string]string{"Accept": "application/json", "Accept-Encoding": "gzip", "User-Agent": "perfloop", "X-Scope-Orgid": "team-a"} {
		if got := seen.Get(name); got != want {
			t.Errorf("upstream saw %s=%q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"X-Forwarded-Host", "Forwarded", "Cookie", "Authorization", link.RequestIDHeader} {
		if _, ok := seen[http.CanonicalHeaderKey(name)]; ok {
			t.Errorf("upstream saw %s=%q; want it dropped", name, seen.Get(name))
		}
	}
}

// An upstream's Perfloop-* response headers are stripped before the answer
// crosses the tunnel, so an upstream cannot speak as Perfloop.
func TestUpstreamCannotForgePerfloopResponseHeaders(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Perfloop-Relay-Error", "1")
		w.Header().Set("Perfloop-Request-Id", "forged/id")
		w.Header().Set("Perfloop-Tenant", "1")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}))
	defer provider.Close()
	term := newTerminator(t, Upstream{Name: "vm", Kind: "prometheus", URL: provider.URL})
	term.run(t)
	term.waitTunnels(t, tunnels)
	resp := term.read(t, "vm", "/api/v1/query")
	if resp.status != http.StatusOK || resp.header.Get("Content-Type") != "application/json" {
		t.Fatalf("read: %+v", resp)
	}
	for name := range resp.header {
		if len(name) >= 9 && name[:9] == "Perfloop-" {
			t.Errorf("forged %s=%q crossed the tunnel", name, resp.header.Get(name))
		}
	}
}

// A redirect from the upstream is not followed: the read fails as an
// upstream error, and the redirect's target sees no request.
func TestUpstreamRedirectIsRefusedNotFollowed(t *testing.T) {
	var followed atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/elsewhere/", func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
		io.WriteString(w, "moved")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/elsewhere"+req.URL.Path, http.StatusFound)
	})
	provider := httptest.NewServer(mux)
	defer provider.Close()
	term := newTerminator(t, Upstream{Name: "vm", Kind: "prometheus", URL: provider.URL})
	term.run(t)
	term.waitTunnels(t, tunnels)
	if resp := term.read(t, "vm", "/api/v1/query"); resp.status != http.StatusBadGateway || followed.Load() != 0 {
		t.Fatalf("redirect: status=%d followed=%d; want 502 and no follow", resp.status, followed.Load())
	}
}

// One tunnel carries at most eight reads at a time; the HTTP/2 settings the
// relay announces make the ninth wait, and the upstream never sees it while
// eight are in flight.
func TestOneTunnelCarriesAtMostEightReadsAtOnce(t *testing.T) {
	var inFlight atomic.Int64
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		inFlight.Add(1)
		select {
		case <-release:
		case <-req.Context().Done():
		}
		io.WriteString(w, "ok")
	}))
	defer provider.Close()
	term := newTerminator(t, Upstream{Name: "vm", Kind: "prometheus", URL: provider.URL})
	term.run(t)
	term.waitTunnels(t, tunnels)
	cc := term.live()[0]
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// The client refuses to open a ninth stream while eight are active, or
	// waits for a slot; either way the upstream never sees a ninth read in
	// flight.
	var refused atomic.Int64
	var wg sync.WaitGroup
	for range maxStreams + 1 {
		wg.Go(func() {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://vm/api/v1/query", http.NoBody)
			req.Host = "vm"
			resp, err := cc.RoundTrip(req)
			if err != nil {
				refused.Add(1)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for inFlight.Load() < int64(maxStreams) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := inFlight.Load(); n != int64(maxStreams) {
		t.Fatalf("upstream saw %d reads in flight, want exactly %d", n, maxStreams)
	}
	if active := cc.State().StreamsActive; active != maxStreams {
		t.Fatalf("tunnel has %d active streams, want %d", active, maxStreams)
	}
	close(release)
	wg.Wait()
	if n := inFlight.Load(); n+refused.Load() != int64(maxStreams)+1 || refused.Load() > 1 {
		t.Fatalf("upstream saw %d reads in total and %d were refused; want %d in all", n, refused.Load(), maxStreams+1)
	}
}
