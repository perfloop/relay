package relay

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// An upstream failure is logged without the request URL: the query string
// stays out of the audit line, as it does on every other decision.
func TestUpstreamErrorIsLoggedWithoutTheQuery(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	term := newTerminator(t, Upstream{Name: "vm", Kind: "prometheus", URL: provider.URL})
	var log bytes.Buffer
	var logMu sync.Mutex
	r, err := New(term.cfg, slog.New(slog.NewJSONHandler(&lockedWriter{mu: &logMu, w: &log}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	term.start(t, r)
	term.waitTunnels(t, tunnels)
	provider.Close()
	if resp := term.read(t, "vm", "/api/v1/query?query=secret_metric"); resp.status != http.StatusBadGateway {
		t.Fatalf("read with the upstream down: %+v", resp)
	}
	logMu.Lock()
	lines := log.String()
	logMu.Unlock()
	if !strings.Contains(lines, `"decision":"upstream-error"`) {
		t.Fatalf("no upstream-error line in %s", lines)
	}
	if strings.Contains(lines, "secret_metric") || strings.Contains(lines, "query=") {
		t.Fatalf("the query string reached the log: %s", lines)
	}
}
