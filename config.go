package relay

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/textproto"
	"net/url"
	"os"
	"strings"

	"github.com/perfloop/relay/link"
	"go.yaml.in/yaml/v3"
	"golang.org/x/net/http/httpguts"
)

// Config is the YAML shape of perfloop-relay. `${VAR}` references in the file
// expand from the environment before parsing, so the token and upstream
// credentials can come from a Kubernetes Secret; an unset variable fails at
// start, not at the first read.
type Config struct {
	// API is the URL of the Perfloop API the relay opens its tunnels to, for
	// example https://app.perfloop.ai. The relay reaches it through
	// HTTPS_PROXY when that is set; such a proxy must carry WebSockets.
	API string `yaml:"api"`
	// APICA is an optional PEM file trusted for the API certificate instead
	// of the system roots, for networks whose egress proxy re-signs TLS.
	APICA string `yaml:"api_ca"`
	// Token is the operator-issued relay token for this tenant.
	Token string `yaml:"token"`
	// Upstreams lists the sources this relay may read.
	Upstreams []Upstream `yaml:"upstreams"`
	// LogLevel is debug, info, warn, or error.
	LogLevel string `yaml:"log_level"`
}

// Upstream is one source inside the customer network.
type Upstream struct {
	// Name is the `relay://<name>` endpoint that Setup registers. It is one
	// lowercase DNS label.
	Name string `yaml:"name"`
	// Kind selects the read routes the relay permits; routes.go lists the
	// kinds (today prometheus, victoriametrics, loki, and pprof).
	Kind string `yaml:"kind"`
	// URL is the provider base: scheme, host, port, and an optional path
	// prefix. The relay appends the proxy's request path to it. A pprof
	// upstream has Targets instead.
	URL string `yaml:"url"`
	// Targets are the processes of a pprof upstream: target name to the
	// http(s) base of that process's net/http/pprof handler. Perfloop names
	// one target per read as the first path segment; a name not listed here
	// is refused. Each name is one lowercase DNS label.
	Targets map[string]string `yaml:"targets"`
	// MaxSeconds caps the `seconds` a pprof read may ask for, so a CPU
	// profile cannot run longer than the customer allows. Zero means
	// DefaultMaxSeconds. The bound is link.MaxProfileSeconds.
	MaxSeconds int `yaml:"max_seconds"`
	// MaxConcurrent caps the pprof reads in flight on this upstream across
	// all of its targets, per relay process; a further read is refused at
	// once with status 429. Perfloop spreads reads over every relay a tenant
	// runs with the same token, so N relay processes admit N times this cap.
	// The customer controls that count. Zero means DefaultMaxConcurrent. The
	// bound is MaxConcurrentBound.
	MaxConcurrent int `yaml:"max_concurrent"`
	// Headers are added to every upstream request, for example an
	// Authorization header holding a read token. They replace any header of
	// the same name from the proxy.
	Headers map[string]string `yaml:"headers"`
	// CA is an optional PEM file trusted for this upstream's TLS certificate
	// instead of the system roots.
	CA string `yaml:"ca"`
}

const (
	// DefaultMaxSeconds is the `seconds` cap of a pprof upstream that sets
	// none: net/http/pprof's own default CPU profile length. The largest cap
	// a config may set is link.MaxProfileSeconds.
	DefaultMaxSeconds = 30
	// DefaultMaxConcurrent is the in-flight cap of a pprof upstream that
	// sets none: one profile at a time per upstream.
	DefaultMaxConcurrent = 1
	// MaxConcurrentBound is the largest max_concurrent a config may set. It
	// is a config bound on how many processes profile at once, not a tunnel
	// property: the relay keeps two tunnels of maxStreams each.
	MaxConcurrentBound = 8
)

// Load reads, expands, and strictly parses a relay configuration. New
// validates it.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	expanded, err := link.ExpandEnv(data)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("relay config: %w", err)
	}
	return cfg, nil
}

// Validate checks the configuration without touching the network.
func (c Config) Validate() error {
	if _, err := c.apiURL(); err != nil {
		return err
	}
	if c.Token == "" {
		return errors.New("token is required")
	}
	if len(c.Upstreams) == 0 {
		return errors.New("at least one upstream is required")
	}
	if _, err := c.level(); err != nil {
		return fmt.Errorf("log_level: %w", err)
	}
	names := make(map[string]bool, len(c.Upstreams))
	for _, up := range c.Upstreams {
		if !link.ValidUpstreamName(up.Name) {
			return fmt.Errorf("upstream name %q must be one lowercase DNS label", up.Name)
		}
		if names[up.Name] {
			return fmt.Errorf("upstream name %q is repeated", up.Name)
		}
		names[up.Name] = true
		if _, ok := base(up.Kind); !ok {
			return fmt.Errorf("upstream %s: kind %q is not supported", up.Name, up.Kind)
		}
		if up.Kind == "pprof" {
			if up.URL != "" {
				return fmt.Errorf("upstream %s: a pprof upstream names targets, not a url", up.Name)
			}
			if len(up.Targets) == 0 {
				return fmt.Errorf("upstream %s: at least one target is required", up.Name)
			}
			for target, raw := range up.Targets {
				if !link.ValidUpstreamName(target) {
					return fmt.Errorf("upstream %s: target name %q must be one lowercase DNS label", up.Name, target)
				}
				if !validBase(raw) {
					return fmt.Errorf("upstream %s: target %s: url must be an http(s) base without query, fragment, user info, or trailing slash", up.Name, target)
				}
			}
			if up.MaxSeconds < 0 || up.MaxSeconds > link.MaxProfileSeconds {
				return fmt.Errorf("upstream %s: max_seconds must be between 1 and %d", up.Name, link.MaxProfileSeconds)
			}
			if up.MaxConcurrent < 0 || up.MaxConcurrent > MaxConcurrentBound {
				return fmt.Errorf("upstream %s: max_concurrent must be between 1 and %d", up.Name, MaxConcurrentBound)
			}
		} else {
			if up.Targets != nil || up.MaxSeconds != 0 || up.MaxConcurrent != 0 {
				return fmt.Errorf("upstream %s: targets, max_seconds, and max_concurrent apply only to kind pprof", up.Name)
			}
			if !validBase(up.URL) {
				return fmt.Errorf("upstream %s: url must be an http(s) base without query, fragment, user info, or trailing slash", up.Name)
			}
		}
		// Header names and values must be ones the HTTP client will send, and
		// one wire header must have one value: `Authorization` and
		// `authorization` are the same header on the wire.
		seen := make(map[string]string, len(up.Headers))
		for name, value := range up.Headers {
			if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
				return fmt.Errorf("upstream %s: header %q is not a valid HTTP header", up.Name, name)
			}
			canonical := textproto.CanonicalMIMEHeaderKey(name)
			if other, dup := seen[canonical]; dup {
				return fmt.Errorf("upstream %s: headers %q and %q are the same header", up.Name, other, name)
			}
			seen[canonical] = name
		}
	}
	return nil
}

// level parses LogLevel; empty is info.
func (c Config) level() (slog.Level, error) {
	var level slog.Level
	if c.LogLevel == "" {
		return level, nil
	}
	err := level.UnmarshalText([]byte(c.LogLevel))
	return level, err
}

// Level is the configured log level. Validate refuses any value that does
// not parse, so on a validated configuration this is exact; before that, an
// unparsable value reads as info and Validate names it.
func (c Config) Level() slog.Level {
	level, _ := c.level()
	return level
}

// validBase reports whether raw is an http(s) base the relay may append a
// route to: scheme, host, port, and an optional path prefix, nothing else.
func validBase(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && !strings.HasSuffix(u.Path, "/")
}

// apiURL is the API origin: an https URL with a host and nothing else. The
// token travels only inside TLS.
func (c Config) apiURL() (*url.URL, error) {
	u, err := url.Parse(c.API)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("api must be an https origin such as https://app.perfloop.ai")
	}
	return u, nil
}
