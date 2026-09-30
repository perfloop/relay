package relay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/perfloop/relay/link"
)

func TestLoadExpandsSecretsAndValidates(t *testing.T) {
	t.Setenv("RELAY_TEST_TOKEN", "tenant-seven")
	t.Setenv("RELAY_TEST_VM_TOKEN", "vm-read")
	path := filepath.Join(t.TempDir(), "relay.yaml")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`api: https://app.perfloop.ai
api_ca: /etc/perfloop-relay/api-ca.pem
token: ${RELAY_TEST_TOKEN}
upstreams:
  - name: vm
    kind: victoriametrics
    url: http://vmselect.monitoring.svc:8481/select/0/prometheus
    headers:
      Authorization: Bearer ${RELAY_TEST_VM_TOKEN}
  - name: logs
    kind: loki
    url: https://loki-gateway.monitoring.svc
    ca: /etc/perfloop-relay/loki-ca.pem
  - name: go
    kind: pprof
    targets:
      api-1: http://10.0.0.1:6060
      api-2: http://10.0.0.2:6060/internal
    max_seconds: 10
    max_concurrent: 2
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "tenant-seven" || cfg.Upstreams[0].Headers["Authorization"] != "Bearer vm-read" || cfg.Upstreams[1].CA == "" {
		t.Fatalf("loaded %+v", cfg)
	}
	if go1 := cfg.Upstreams[2]; len(go1.Targets) != 2 || go1.Targets["api-2"] != "http://10.0.0.2:6060/internal" || go1.MaxSeconds != 10 || go1.MaxConcurrent != 2 {
		t.Fatalf("loaded pprof upstream %+v", go1)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	write("api: https://app.perfloop.ai\ntoken: ${RELAY_TEST_MISSING}\nupstreams: [{name: vm, kind: prometheus, url: http://vm:8428}]\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "RELAY_TEST_MISSING") {
		t.Fatalf("unset variable: %v", err)
	}
	write("api: https://app.perfloop.ai\ntoken: t\nupstreams: [{name: vm, kind: prometheus, url: http://vm:8428, extra: 1}]\n")
	if _, err := Load(path); err == nil {
		t.Fatal("unknown field accepted")
	}
	base := Config{API: "https://app.perfloop.ai", Token: "t", Upstreams: []Upstream{{Name: "vm", Kind: "prometheus", URL: "http://vm:8428"}}}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Config){
		"api without scheme": func(c *Config) { c.API = "app.perfloop.ai:443" },
		"api with path":      func(c *Config) { c.API = "https://app.perfloop.ai/v1" },
		"api with user":      func(c *Config) { c.API = "https://u:p@app.perfloop.ai" },
		"api over http":      func(c *Config) { c.API = "http://app.perfloop.ai" },
		"missing token":      func(c *Config) { c.Token = "" },
		"unknown log level":  func(c *Config) { c.LogLevel = "nonsense" },
		"no upstreams":       func(c *Config) { c.Upstreams = nil },
		"uppercase name":     func(c *Config) { c.Upstreams[0].Name = "VM" },
		"dotted name":        func(c *Config) { c.Upstreams[0].Name = "vm.internal" },
		"repeated name":      func(c *Config) { c.Upstreams = append(c.Upstreams, c.Upstreams[0]) },
		"unknown kind":       func(c *Config) { c.Upstreams[0].Kind = "pyroscope" },
		"url with query":     func(c *Config) { c.Upstreams[0].URL = "http://vm:8428/?x=1" },
		"url with user":      func(c *Config) { c.Upstreams[0].URL = "http://u:p@vm:8428" },
		"url trailing slash": func(c *Config) { c.Upstreams[0].URL = "http://vm:8428/" },
		"relative url":       func(c *Config) { c.Upstreams[0].URL = "vm:8428" },
		"header with newline": func(c *Config) {
			c.Upstreams[0].Headers = map[string]string{"Authorization": "Bearer x\r\nEvil: y"}
		},
		"header name outside the grammar": func(c *Config) {
			c.Upstreams[0].Headers = map[string]string{"X/Token": "x"}
		},
		"header value outside the grammar": func(c *Config) {
			c.Upstreams[0].Headers = map[string]string{"X-Token": "x\x00y"}
		},
		"one header spelled twice": func(c *Config) {
			c.Upstreams[0].Headers = map[string]string{"Authorization": "Bearer x", "authorization": "Bearer y"}
		},
		"targets on a metrics upstream":     func(c *Config) { c.Upstreams[0].Targets = map[string]string{"a": "http://a:6060"} },
		"max_seconds on a metrics upstream": func(c *Config) { c.Upstreams[0].MaxSeconds = 10 },
		"pprof with url":                    func(c *Config) { c.Upstreams[0] = pprof(func(u *Upstream) { u.URL = "http://a:6060" }) },
		"pprof without targets":             func(c *Config) { c.Upstreams[0] = pprof(func(u *Upstream) { u.Targets = nil }) },
		"pprof target name with a dot": func(c *Config) {
			c.Upstreams[0] = pprof(func(u *Upstream) { u.Targets = map[string]string{"api.1": "http://a:6060"} })
		},
		"pprof target name with a slash": func(c *Config) {
			c.Upstreams[0] = pprof(func(u *Upstream) { u.Targets = map[string]string{"api/1": "http://a:6060"} })
		},
		"pprof target url with query": func(c *Config) {
			c.Upstreams[0] = pprof(func(u *Upstream) { u.Targets = map[string]string{"a": "http://a:6060/?x=1"} })
		},
		"pprof target url with user": func(c *Config) {
			c.Upstreams[0] = pprof(func(u *Upstream) { u.Targets = map[string]string{"a": "http://u:p@a:6060"} })
		},
		"pprof target url with trailing slash": func(c *Config) {
			c.Upstreams[0] = pprof(func(u *Upstream) { u.Targets = map[string]string{"a": "http://a:6060/"} })
		},
		"pprof max_seconds over the bound": func(c *Config) {
			c.Upstreams[0] = pprof(func(u *Upstream) { u.MaxSeconds = link.MaxProfileSeconds + 1 })
		},
		"pprof max_seconds negative": func(c *Config) { c.Upstreams[0] = pprof(func(u *Upstream) { u.MaxSeconds = -1 }) },
		"pprof max_concurrent over the bound": func(c *Config) {
			c.Upstreams[0] = pprof(func(u *Upstream) { u.MaxConcurrent = MaxConcurrentBound + 1 })
		},
	} {
		cfg := base
		cfg.Upstreams = append([]Upstream(nil), base.Upstreams...)
		change(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// pprof is a valid pprof upstream with one change applied.
func pprof(change func(*Upstream)) Upstream {
	up := Upstream{Name: "go", Kind: "pprof", Targets: map[string]string{"api-1": "http://10.0.0.1:6060"}}
	change(&up)
	return up
}

func TestValidPprofUpstream(t *testing.T) {
	cfg := Config{API: "https://app.perfloop.ai", Token: "t", Upstreams: []Upstream{pprof(func(*Upstream) {})}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
