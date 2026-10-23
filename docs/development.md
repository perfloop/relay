# Development

```sh
go test -race ./...
go vet ./...
golangci-lint run ./...
```

The module has three runtime dependencies: `github.com/coder/websocket` (the
tunnel's WebSocket), `go.yaml.in/yaml/v3` (the configuration file), and
`golang.org/x/net` (HTTP/2 inside the tunnel and header validation).
`go.uber.org/goleak` is used by the tests only; it is not linked into the
binary. The line count in the README is `cat relay.go connect.go config.go
routes.go link/link.go cmd/perfloop-relay/main.go | wc -l`.

`relay_test.go` runs the relay against a terminator of the same wire shape
as the Perfloop API's: it proves the route allowlist, that upstream
credentials come from the configuration and Perfloop's do not reach the
upstream, the oversize cut, the egress-proxy split, the token's confinement to
the configured API, and the read id's presence in the log and absence from the
upstream request. The Perfloop API's own tests run its real terminator against
a relay of this shape; `link/` is the package both sides share.

`connect_test.go` uses that same HTTP/2-over-WebSocket tunnel for CONNECT.
It waits for `200` before sending TLS bytes, validates a code-host certificate
at the test proxy only, and checks that provider credentials cross only inside
TLS. It also checks an unauthenticated `401`, untrusted and wrong-name TLS,
an opaque stream larger than 32 MiB in each direction, TCP half-close,
cancellation, tunnel closure, stopping `Run`, marked dial failure, and a
broken stream without replay after a TCP reset. The HTTP/2 client owns its
upload pipe: it closes that pipe on cancellation so an idle body read cannot
delay RST_STREAM after `RoundTrip` has returned response headers.
