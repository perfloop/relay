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
	"errors"
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
	// ErrorHeader marks a response that is not the provider's: Perfloop made
	// it (Unavailable) or the relay refused the read (Refused). A reader can
	// tell either from provider bytes; the API and the proxy act only on
	// Unavailable, and a relay refusal reaches the adapter with its status.
	ErrorHeader = "Perfloop-Relay-Error"
	// Unavailable is the value Perfloop sets when no relay could serve the
	// read: no tunnel, or a token the replica does not recognize.
	Unavailable = "1"
	// Refused is the value the relay sets on its own refusals: an unknown
	// upstream or target, a route or query outside the allowlist, or the
	// in-flight cap. The status says which.
	Refused = "relay"
	// MaxProfileSeconds is the longest `seconds` a pprof read may ask for on
	// either end: the relay refuses a larger `max_seconds` in its config, and
	// Perfloop's pprof adapter refuses a longer capture before the tunnel. A
	// profile read crosses two one-minute deadlines, the controller's remote
	// client and the proxy's read; 45 leaves them 15 seconds for the tunnel
	// and the bytes.
	MaxProfileSeconds = 45
)

// hopHeaders never cross a tunnel in either direction (RFC 9110 § 7.6.1),
// nor do proxy credentials, which belong to the hop that used them.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "Transfer-Encoding", "Upgrade", "Te", "Trailer"}

// ForwardedHeaders are the request headers a relay copies from a read onto
// the upstream request. They are exactly what Perfloop sends on a read:
// Accept and X-Scope-OrgID (the Loki tenant) from its provider adapters,
// Accept-Encoding and User-Agent from its HTTP client. Every other request
// header stops at the relay; the customer's configured headers are set after
// these and replace any of the same name.
var ForwardedHeaders = []string{"Accept", "Accept-Encoding", "User-Agent", "X-Scope-OrgID"}

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
	// os.Expand drops an unterminated `${` and everything after it; a file
	// with one is a mistake to report, never a shorter file to run.
	for rest := string(data); ; {
		i := strings.Index(rest, "${")
		if i < 0 {
			break
		}
		rest = rest[i+2:]
		if !strings.Contains(rest, "}") {
			return nil, errors.New("config has an unterminated ${ reference")
		}
	}
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
