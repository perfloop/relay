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
binary. The line count in the README is `cat relay.go config.go routes.go
link/link.go cmd/perfloop-relay/main.go | wc -l`.

`relay_test.go` runs the relay against a terminator of the same wire shape
as the Perfloop API's: it proves the route allowlist, that upstream
credentials come from the configuration and Perfloop's do not reach the
upstream, the oversize cut, the egress-proxy split, the token's confinement to
the configured API, and the read id's presence in the log and absence from the
upstream request. The Perfloop API's own tests run its real terminator against
a relay of this shape; `link/` is the package both sides share.
