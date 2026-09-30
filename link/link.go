// Package link is what the two ends of a relay tunnel share: the wire shape
// (the relay that a customer runs and the Perfloop API that terminates the
// tunnel must agree on it) and the one way both read their configuration. It
// holds nothing else, so the relay is published on its own and Perfloop
// depends on this module, never the reverse. The relay's tests run against a
// terminator of this shape in relay_test.go; the Perfloop API's tests run its
// real terminator against a relay of this shape.
//
// The tunnel is a WebSocket the relay opens to the API host over HTTPS, so a
// managed balancer terminates TLS and any corporate proxy carries it. Inside
// the socket the roles turn around: Perfloop is the HTTP/2 client and the
// relay is the HTTP/2 server, so every validated read is an ordinary HTTP
// request from Perfloop to the relay and the relay never sends a request into
// Perfloop.
package link

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
)

const (
	// TunnelPath is the API route a relay opens its tunnel on, with its token
	// as a bearer credential.
	TunnelPath = "/v1/relays/tunnel"
	// Subprotocol is the WebSocket subprotocol both ends name, so a stray
	// WebSocket client on the route is refused at the handshake.
	Subprotocol = "perfloop-relay"
	// RequestIDHeader names the Perfloop work behind one read as
	// `<session id>/<tool call ref>` (the tool call's transcript frame and
	// id), so the customer can join the relay's
	// audit line to the session transcript that asked for it. Perfloop sets
	// it, its tunnel token covers it, and the relay logs it and drops it
	// before the upstream request. It is absent on a read no tool call made.
	RequestIDHeader = "Perfloop-Request-Id"
)

// hopHeaders never cross a tunnel in either direction (RFC 9110 § 7.6.1).
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Connection", "Transfer-Encoding", "Upgrade", "Te", "Trailer"}

// StripHop removes the hop-by-hop headers from a request or response header
// that is about to cross a tunnel. Both ends call it on the way in and on the
// way out.
func StripHop(header http.Header) {
	for _, name := range hopHeaders {
		header.Del(name)
	}
}

// ValidUpstreamName reports whether name is one lowercase DNS label: 1 to 63
// characters of a-z, 0-9, and -, with no leading or trailing -. It is the one
// rule for the name: Setup accepts `relay://<name>` only for such names,
// Perfloop forwards only such names, and the relay refuses other names at
// startup so a typo fails early instead of never matching.
func ValidUpstreamName(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, char := range []byte(name) {
		if char != '-' && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

// ExpandEnv replaces `${VAR}` and `$VAR` in a configuration file with
// environment values, so a Kubernetes Secret can supply a token or header.
// Every referenced variable must be set; the error names the missing ones. A
// literal `$` cannot appear in these files; put such a value in a variable.
func ExpandEnv(data []byte) ([]byte, error) {
	missing := map[string]bool{}
	out := os.Expand(string(data), func(name string) string {
		value, ok := os.LookupEnv(name)
		if !ok {
			missing[name] = true
		}
		return value
	})
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for name := range missing {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("config references unset environment variables: %s", strings.Join(names, ", "))
	}
	return []byte(out), nil
}
