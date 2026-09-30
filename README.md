# Perfloop relay

`perfloop-relay` is a small program you run inside your own network so that
Perfloop can read telemetry sources the public internet cannot reach: a
Prometheus, VictoriaMetrics, or Loki that only your cluster can see. This
repository is the complete source of what runs in your network. The image
Perfloop publishes is built from it, by the workflow in this repository, and
is tagged with the commit it was built from.

The relay opens outbound tunnels to the Perfloop API and answers reads that
Perfloop already validated, by forwarding each one to an upstream you named in
its configuration. It collects nothing: it stores no telemetry, pushes none,
opens no inbound port, and never sends a request into Perfloop.

- No credential leaves your network. Provider URLs, hostnames, and the
  headers you configure for an upstream are read from the relay's own
  configuration file and are sent only to that upstream (`relay.go`,
  `forward`). Perfloop does not receive them, and a Perfloop-registered
  `relay://` source has no credential of its own.
- Perfloop cannot push configuration or commands to the relay. The relay reads
  its configuration once, from a file you control, at start (`config.go`,
  `Load`). The only thing that arrives over a tunnel is an HTTP `GET` for a
  route of an upstream named in that file; everything else is refused.

## How the tunnel works

The relay opens a WebSocket to the Perfloop API host over HTTPS
(`relay.go`, `open`):

- The target is `wss://<api host>/v1/relays/tunnel` with the relay token as a
  `Authorization: Bearer` header and the `perfloop-relay` subprotocol
  (`link/link.go`). The API host is the `api` field of the configuration and
  nothing else: a redirect, to any host, ends the handshake instead of being
  followed, so the token is sent only to the configured origin.
- TLS 1.2 or later, trusting the system roots or, when `api_ca` is set, only
  that certificate authority. `HTTPS_PROXY` in the environment is honored for
  the tunnel, so an egress proxy that carries WebSockets is enough; it is not
  applied to upstream requests, which are local.
- Inside the socket the roles turn around: Perfloop is the HTTP/2 client and
  the relay is the HTTP/2 server (`relay.go`, `serve`). Every read is an
  ordinary HTTP request from Perfloop to the relay. The relay never opens a
  stream of its own.
- The relay keeps two tunnels open so the loss of one never leaves you
  without a tunnel, reopens a tunnel that ends with a backoff of at most ten
  seconds, and pings a tunnel that has been silent for thirty seconds; a ping
  with no answer within ten seconds ends the tunnel. At most eight reads are
  in flight on one tunnel; the tunnel's HTTP/2 settings enforce it.
- On `SIGINT` or `SIGTERM` the relay closes every tunnel at once and exits
  (`relay.go`, `Run` and `serve`; `cmd/perfloop-relay/main.go`). A read in
  flight on a closed tunnel fails; Perfloop retries a read whose response had
  not started on another live tunnel of the tenant, which is why the relay
  keeps two, and reports the read as unavailable when none is left. There is
  no drain.

## What the relay forwards, and what it refuses

Every read is checked in `relay.go`, `forward`, before any upstream request is
made. The route table it checks against is `routes.go`, and it is the same
table that `-print-routes` prints, so the printed listing is what the relay
does.

A read is forwarded only when all of these hold:

1. Its host names an upstream in the configuration. Any other host answers
   `404 unknown upstream`.
2. It is a `GET` with no body. Anything else answers `405 only GET without a
   body is relayed`.
3. Its path is clean: absolute, with no `.`, `..`, or repeated slashes.
   Otherwise `400 path is not clean`.
4. Its path is the provider's API base followed by exactly one documented read
   route, with nothing before the base and nothing after the route. Otherwise
   `403 route is not a documented read`.

The documented read routes, per upstream kind:

| Kind | Base | Read routes |
| --- | --- | --- |
| `prometheus`, `victoriametrics` | `/api/v1/` | `query`, `query_range`, `labels`, `label/{name}/values`, `series`, `metadata` |
| `loki` | `/loki/api/v1/` | `query`, `query_range`, `labels`, `label/{name}/values` |

`{name}` is one non-empty path segment. Admin, write, push, and delete routes
are not in the table and are refused before any upstream request; the tests in
`routes_test.go` and `relay_test.go` include the cases `admin/tsdb/delete_series`,
`admin/tsdb/snapshot`, `write`, `push`, and a `..` in the path.

A forwarded request is built as: the upstream `url` plus the read's path,
the read's query string unchanged, the end-to-end headers Perfloop sent
(hop-by-hop headers are removed), then the upstream's configured `headers`,
which replace any header of the same name. The `Perfloop-Request-Id` header
is removed before the upstream request. The upstream response's status,
headers, and body are returned to Perfloop as they are. A redirect from the
upstream is not followed: the read fails with `502 upstream request failed`,
like any other upstream error.

Bounds on one read: two minutes end to end, and a response body of at most
32 MiB. A larger body aborts the read so that Perfloop sees a broken read,
never a short body that looks complete.

## What the relay does not do

- It does not inspect or rewrite upstream responses. There is no scanning
  of response bodies for credentials and no redaction; the body Perfloop
  receives is the body the upstream sent (`relay.go`, `forward`, the
  `io.Copy` after `WriteHeader`). Your protections are the ones you already
  own: give each upstream a read-only, query-scoped token in its `headers`,
  and use an upstream that does not echo request headers into responses. Every
  supported kind satisfies this by default.
- It has no inbound listener. It does not bind a port. The only network
  connections it opens are the tunnels to the configured `api` and the
  requests to the configured upstreams.
- It takes no configuration push, no remote update, and no command. There is
  no shell, no exec route, and no route that changes its state. A new relay
  version is a new image you choose to roll.
- It cannot widen what Perfloop may read. The route table above is a control
  you own; Perfloop's own validation of each read happens before the read
  reaches the relay, and the relay does not repeat or replace it.
- When the relay token is revoked in Perfloop Setup, the Perfloop API ends
  its open tunnels within thirty seconds (the API checks each tunnel's key on
  that schedule; the relay takes no part in it), and every reconnect after
  that is refused with `401 Unauthorized`, which the relay logs as
  `perfloop refused tunnel`.

## The relay token

A relay token is a Perfloop API key with the `relay` scope. A tenant admin
creates it in Setup, under API keys, as a relay token, and revokes it there.
The plaintext is shown once; Perfloop stores only its hash. The token lets a
relay answer that tenant's validated reads and nothing else: the Perfloop API
refuses it as a bearer everywhere but the tunnel route, so a token held in
your network reads no Perfloop data. More than one relay may run with the
same token; the API uses any live tunnel of the tenant and holds at most eight
tunnels per tenant on each replica, refusing a further one with status 429
until one ends.

## Configuration

The configuration is one YAML file. `${VAR}` and `$VAR` references are
expanded from the environment before parsing, so the token and any upstream
credential can live in a Kubernetes Secret; a reference to an unset variable
fails at start, and a literal `$` cannot appear in the file. Unknown fields
are refused. Every field:

```yaml
# The Perfloop API origin the relay opens its tunnels to. It must be an
# https origin with nothing after the host.
api: https://app.perfloop.ai

# Optional. A PEM file trusted for the API certificate instead of the system
# roots, for networks whose egress proxy re-signs TLS.
api_ca: /etc/perfloop-relay/api-ca.pem

# The relay token created in Setup. Required.
token: ${PERFLOOP_RELAY_TOKEN}

# Optional. debug, info (the default), warn, or error.
log_level: info

# The sources this relay may read. At least one is required.
upstreams:
  - # The name Setup registers as the endpoint `relay://<name>`. One
    # lowercase DNS label: a-z, 0-9, and -, up to 63 characters, no
    # leading or trailing -. Names must be unique in the file.
    name: vm
    # prometheus, victoriametrics, or loki. Selects the read routes above.
    kind: victoriametrics
    # The provider base: scheme, host, port, and an optional path prefix.
    # No user info, query, fragment, or trailing slash. The read's path is
    # appended to it.
    url: http://vmselect.monitoring.svc:8481/select/0/prometheus
    # Optional. Headers added to every request to this upstream; they
    # replace any header of the same name that Perfloop sent. Values are
    # never logged or printed.
    headers:
      Authorization: Bearer ${VM_READ_TOKEN}
    # Optional. A PEM file trusted for this upstream's TLS certificate
    # instead of the system roots.
    ca: /etc/perfloop-relay/vm-ca.pem
  - name: logs
    kind: loki
    url: https://loki-gateway.monitoring.svc
```

The validation rules are in `config.go`, `Validate`; `config_test.go` lists
every refused shape.

## Running it

```sh
perfloop-relay -config /etc/perfloop-relay/relay.yaml
```

`-config` defaults to `/etc/perfloop-relay/relay.yaml`. Logs are JSON lines
on standard output.

### `-print-routes`

```sh
perfloop-relay -config relay.yaml -print-routes
```

loads and validates the configuration and prints, per upstream, its kind,
its URL, the names of its configured headers, and every request the relay
will forward, then exits. An invalid configuration exits non-zero with the
validation error. Header values and the token are not printed. Example:

```
upstream vm
  kind    victoriametrics
  url     http://vm.internal:8428/select/0/prometheus
  headers Authorization, X-Scope-OrgID (values not shown)
  forwards GET, with the query string unchanged, to:
    http://vm.internal:8428/select/0/prometheus/api/v1/query
    http://vm.internal:8428/select/0/prometheus/api/v1/query_range
    http://vm.internal:8428/select/0/prometheus/api/v1/labels
    http://vm.internal:8428/select/0/prometheus/api/v1/label/{name}/values
    http://vm.internal:8428/select/0/prometheus/api/v1/series
    http://vm.internal:8428/select/0/prometheus/api/v1/metadata
```

Run it before you roll the relay, and keep its output with your change record:
it is the list of everything the relay can be asked to do.

### The audit log

The relay writes one line per read, `msg: "relay read"`, with these fields:

| Field | Meaning |
| --- | --- |
| `upstream` | The upstream name the read asked for. |
| `method`, `path` | The method and path as Perfloop sent them. The query string is not logged, in this field or in `error`. |
| `request_id` | The Perfloop work behind the read, `<session id>/<tool call ref>`, from the `Perfloop-Request-Id` header. Empty for a read no tool call made, such as a connect check from Setup. |
| `status` | The status the relay answered. |
| `decision` | The read route that was forwarded (`query`, `labels`, ...), or why the read was refused: `unknown-upstream`, `method`, `path`, `route`, `upstream-error`, or `truncated`. |
| `ms` | Time spent on the read. |
| `error` | The upstream error, if any, without the request URL (`relay.go`, `forward`). |

`request_id` lets you join a line in your log to the session transcript in
Perfloop that asked for the read. The relay logs it and removes it before the
upstream request, so your provider never sees a Perfloop identifier. Tunnel
life is logged as `tunnel open` and `tunnel ended`, with the error and the
retry delay.

## The image

The image is published by `.github/workflows/image.yml` on every push to
`main`, at

```
us-central1-docker.pkg.dev/perfloop-public-prod/perfloop-images/relay
```

tagged with the full and the short SHA of the commit it was built from, so a
tag names a commit of this repository. Pin the image by digest in your
manifests. To verify that the image is what this repository builds:

1. Open the `Image` workflow run for that commit under Actions in this
   repository. Its summary lists the tags and the manifest digest.
2. Resolve the tag yourself and compare the digest:

   ```sh
   docker buildx imagetools inspect us-central1-docker.pkg.dev/perfloop-public-prod/perfloop-images/relay:<commit sha>
   ```

3. Compare the binary, not the image. The binary is reproducible: the Go
   toolchain is pinned by digest in the `Dockerfile`, the dependencies by
   `go.sum`, and the build uses `-trimpath -buildvcs=false -ldflags="-s -w"`,
   so the same commit gives the same bytes. The image digest is not
   reproducible (layer metadata and the SBOM attestation differ between
   builds), so a rebuilt image will not match the published digest. From the
   commit's checkout:

   ```sh
   image=us-central1-docker.pkg.dev/perfloop-public-prod/perfloop-images/relay@<digest>
   id="$(docker create "$image")"
   docker cp "$id:/usr/local/bin/perfloop-relay" published-relay
   docker rm "$id" >/dev/null
   docker run --rm -v "$PWD:/src" -w /src \
     golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 \
     sh -c 'CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /src/local-relay ./cmd/perfloop-relay'
   sha256sum published-relay local-relay
   ```

   The two sums are equal.

The image runs the binary as `nonroot` on a distroless base with nothing
else in it. A published image carries an SPDX SBOM attestation.

A Kubernetes Deployment needs: the configuration file in a ConfigMap mounted
at `/etc/perfloop-relay/relay.yaml`, the token in a Secret exposed as the
environment variable the file references, and outbound access to the Perfloop
API host on 443 and to the upstreams. Nothing needs to reach the relay.

## Development

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

## License

Apache License 2.0. See `LICENSE`.
