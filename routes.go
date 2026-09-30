package relay

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/perfloop/relay/link"
)

// base returns the provider API base for a supported kind. It is the one
// list of kinds the relay serves; Validate refuses every other kind.
func base(kind string) (string, bool) {
	switch kind {
	case "prometheus", "victoriametrics":
		return "/api/v1/", true
	case "loki":
		return "/loki/api/v1/", true
	}
	return "", false
}

// labelValues is the one read route with a variable segment: the label name,
// one non-empty path segment.
const labelValues = "label/{name}/values"

// reads lists the documented read routes under the provider API base, per
// kind. It is the one table: route matches a request against it and Routes
// prints it, so what the print shows is what the relay forwards.
func reads(kind string) []string {
	switch kind {
	case "prometheus", "victoriametrics":
		return []string{"query", "query_range", "labels", labelValues, "series", "metadata"}
	case "loki":
		return []string{"query", "query_range", "labels", labelValues}
	}
	return nil
}

// route returns the read route that path names for kind. The path is the
// provider API base and then exactly one documented read route, nothing
// before and nothing else after: the customer's own prefix lives in the
// upstream URL, so a Loki path never reads as a Prometheus one on a shared
// gateway, and admin and write routes are refused before any upstream
// request. Callers pass a clean absolute path.
func route(kind, path string) (string, bool) {
	base, ok := base(kind)
	if !ok {
		return "", false
	}
	rest, ok := strings.CutPrefix(path, base)
	if !ok {
		return "", false
	}
	routes := reads(kind)
	if slices.Contains(routes, rest) {
		return rest, true
	}
	if label, ok := strings.CutPrefix(rest, "label/"); ok && slices.Contains(routes, labelValues) {
		if name, ok := strings.CutSuffix(label, "/values"); ok && name != "" && !strings.Contains(name, "/") {
			return labelValues, true
		}
	}
	return "", false
}

// Routes writes what a validated configuration permits, one upstream at a
// time: the kind, the URL the relay forwards to, the request headers it passes
// from Perfloop, the names of the headers it adds, and every request the relay
// will forward. The relay forwards the
// query string of a permitted route unchanged; the proxy validated it.
// Nothing else is forwarded. Secrets are never written: header values and the
// token stay out. The listing is written whole, in one write, so a failed
// write never leaves a partial listing that reports success.
func Routes(w io.Writer, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	var out strings.Builder
	for i, up := range cfg.Upstreams {
		if i > 0 {
			out.WriteString("\n")
		}
		fmt.Fprintf(&out, "upstream %s\n", up.Name)
		fmt.Fprintf(&out, "  kind    %s\n", up.Kind)
		fmt.Fprintf(&out, "  url     %s\n", up.URL)
		fmt.Fprintf(&out, "  passes  %s from Perfloop; every other request header is dropped\n", strings.Join(link.ForwardedHeaders, ", "))
		headers := slices.Sorted(maps.Keys(up.Headers))
		if len(headers) > 0 {
			fmt.Fprintf(&out, "  headers %s (values not shown)\n", strings.Join(headers, ", "))
		}
		out.WriteString("  forwards GET, with the query string unchanged, to:\n")
		base, _ := base(up.Kind)
		for _, read := range reads(up.Kind) {
			fmt.Fprintf(&out, "    %s%s%s\n", up.URL, base, read)
		}
	}
	_, err := io.WriteString(w, out.String())
	return err
}
