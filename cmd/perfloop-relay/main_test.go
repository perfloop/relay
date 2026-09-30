package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -print-routes prints the relay package's table for a valid config and
// exits with Validate's error for an invalid one, before any tunnel opens.
func TestPrintRoutesPrintsTheTableOrValidatesError(t *testing.T) {
	t.Setenv("PERFLOOP_RELAY_TOKEN", "tenant-seven-token")
	t.Setenv("VM_READ_TOKEN", "vm-read-token")
	var out strings.Builder
	if err := runArgs([]string{"-config", "../../testdata/routes.yaml", "-print-routes"}, &out); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("../../testdata/routes.golden")
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Fatalf("printed:\n%s\nwant:\n%s", out.String(), want)
	}
	bad := filepath.Join(t.TempDir(), "relay.yaml")
	for body, want := range map[string]string{
		"api: https://app.perfloop.ai\ntoken: x\nupstreams:\n  - name: vm\n    kind: pprof\n    url: http://vm.internal\n":                           `kind "pprof" is not supported`,
		"api: https://app.perfloop.ai\ntoken: x\nlog_level: nonsense\nupstreams:\n  - name: vm\n    kind: prometheus\n    url: http://vm.internal\n": `log_level:`,
	} {
		if err := os.WriteFile(bad, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		if err := runArgs([]string{"-config", bad, "-print-routes"}, &out); err == nil || !strings.Contains(err.Error(), want) || out.Len() != 0 {
			t.Fatalf("invalid config: err=%v printed=%q, want %q", err, out.String(), want)
		}
	}
}
