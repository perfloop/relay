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
	// Kind is prometheus, victoriametrics, or loki. It selects the read
	// routes the relay permits.
	Kind string `yaml:"kind"`
	// URL is the provider base: scheme, host, port, and an optional path
	// prefix. The relay appends the proxy's request path to it.
	URL string `yaml:"url"`
	// Headers are added to every upstream request, for example an
	// Authorization header holding a read token. They replace any header of
	// the same name from the proxy.
	Headers map[string]string `yaml:"headers"`
	// CA is an optional PEM file trusted for this upstream's TLS certificate
	// instead of the system roots.
	CA string `yaml:"ca"`
}

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
		u, err := url.Parse(up.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(u.Path, "/") {
			return fmt.Errorf("upstream %s: url must be an http(s) base without query, fragment, user info, or trailing slash", up.Name)
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

// apiURL is the API origin: an https URL with a host and nothing else. The
// token travels only inside TLS.
func (c Config) apiURL() (*url.URL, error) {
	u, err := url.Parse(c.API)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("api must be an https origin such as https://app.perfloop.ai")
	}
	return u, nil
}
