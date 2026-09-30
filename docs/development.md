# Development

```sh
go test -race ./...
go vet ./...
golangci-lint run ./...
```

`relay_test.go` runs the relay against a terminator of the same wire shape
as the Perfloop API's: it proves the route allowlist, that upstream
credentials come from the configuration and Perfloop's do not reach the
upstream, the oversize cut, the egress-proxy split, the token's confinement to
the configured API, and the read id's presence in the log and absence from the
upstream request. The Perfloop API's own tests run its real terminator against
a relay of this shape; `link/` is the package both sides share.
