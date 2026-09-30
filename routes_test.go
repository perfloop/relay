package relay

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// An admitted path is the provider base and exactly one documented read
// route. Nothing before the base (a Loki path holds `/api/v1/`) and nothing
// after it may name another route, so an admin or write route cannot hide on
// either side of a read route's name.
func FuzzRouteNeverAdmitsWritesOrAdmin(f *testing.F) {
	for _, seed := range []string{
		"/api/v1/query", "/select/0/prometheus/api/v1/query_range", "/api/v1/label/x/values", "/admin-cluster/api/v1/labels",
		"/api/v1/admin/tsdb/delete_series", "/api/v1/admin/tsdb/snapshot/api/v1/query", "/api/v1/write/api/v1/labels",
		"/loki/api/v1/push", "/loki/api/v1/push/loki/api/v1/query", "/api/v1/query/../write", "/loki/api/v1/query",
	} {
		f.Add("prometheus", seed)
		f.Add("loki", seed)
	}
	reads := map[string]bool{"query": true, "query_range": true, "labels": true, "series": true, "metadata": true}
	f.Fuzz(func(t *testing.T, kind, path string) {
		name, ok := route(kind, path)
		if !ok {
			return
		}
		base := "/api/v1/"
		if kind == "loki" {
			base = "/loki/api/v1/"
		}
		rest, atStart := strings.CutPrefix(path, base)
		if !atStart {
			t.Fatalf("admitted %s %s as %s", kind, path, name)
		}
		if label, ok := strings.CutPrefix(rest, "label/"); ok {
			labelName, isLabel := strings.CutSuffix(label, "/values")
			if !isLabel {
				t.Fatalf("admitted %s %s as %s", kind, path, name)
			}
			if name != labelValues || labelName == "" || strings.Contains(labelName, "/") {
				t.Fatalf("admitted %s %s as %s", kind, path, name)
			}
			return
		}
		if !reads[rest] || rest != name || (kind == "loki" && (rest == "series" || rest == "metadata")) {
			t.Fatalf("admitted %s %s as %s", kind, path, name)
		}
	})
}

// The route table by example: the base at the start and one read route after
// it; a Loki route is never a Prometheus read on a shared gateway.
func TestRouteAdmitsExactlyBaseAndOneReadRoute(t *testing.T) {
	for _, tc := range []struct{ kind, path, want string }{
		{"prometheus", "/api/v1/query", "query"},
		{"victoriametrics", "/api/v1/label/job/values", labelValues},
		{"loki", "/loki/api/v1/query_range", "query_range"},
		{"prometheus", "/loki/api/v1/query", ""},
		{"prometheus", "/select/0/prometheus/api/v1/query", ""},
		{"loki", "/api/v1/query", ""},
		{"loki", "/loki/api/v1/series", ""},
		{"prometheus", "/api/v1/admin/tsdb/snapshot", ""},
	} {
		if got, ok := route(tc.kind, tc.path); got != tc.want || ok != (tc.want != "") {
			t.Errorf("route(%s, %s) = %q, %v; want %q", tc.kind, tc.path, got, ok, tc.want)
		}
	}
}

// -print-routes shows exactly what the route table admits, from the same
// table, with no secret: the token and header values stay out.
func TestRoutesPrintsTheTableAndNoSecret(t *testing.T) {
	t.Setenv("PERFLOOP_RELAY_TOKEN", "tenant-seven-token")
	t.Setenv("VM_READ_TOKEN", "vm-read-token")
	cfg, err := Load("testdata/routes.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := Routes(&out, cfg); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/routes.golden")
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Fatalf("routes printed:\n%s\nwant:\n%s", out.String(), want)
	}
	if strings.Contains(out.String(), "token") {
		t.Fatal("a secret is printed")
	}
	// A write that fails is the caller's error, never a silent partial print.
	if err := Routes(failingWriter{}, cfg); err == nil || err.Error() != "disk full" {
		t.Fatalf("failed write reported %v", err)
	}
	// The print is of a validated config only, with Validate's own error.
	cfg.Upstreams[0].Kind = "pprof"
	if err := Routes(&out, cfg); err == nil || err.Error() != cfg.Validate().Error() {
		t.Fatalf("invalid config printed: %v", err)
	}
	// Every route the table prints is one route admits, and the only ones.
	for _, kind := range []string{"prometheus", "victoriametrics", "loki"} {
		base, _ := base(kind)
		for _, read := range reads(kind) {
			path := base + strings.ReplaceAll(read, "{name}", "job")
			if name, ok := route(kind, path); !ok || name != read {
				t.Fatalf("printed %s %s is not admitted: %q %v", kind, path, name, ok)
			}
		}
	}
}

// failingWriter fails every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
