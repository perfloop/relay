# Security model

What runs in your network, what it can be asked to do, and what it cannot. Every claim names the code that enforces it.

## Threat model

What the relay defends against:

- Perfloop, or anyone holding a tunnel, reaching anything not in your file.
  A request is answered only for an upstream named in the configuration
  (`relay.go`, `forward`: `r.upstreams[req.Host]`). Telemetry permits only a
  `GET` without a body on a documented read route (`routes.go`, `route`). A
  `codehost` permits only a pathless HTTP/2 `CONNECT` to its configured
  origin's TCP address (`connect.go`, `connect`). The request cannot choose
  a destination. There is no other handler, listener, or route.
- A stolen relay token reading Perfloop data. The token is a bearer for the
  tunnel route only; the Perfloop API refuses it everywhere else. The relay
  sends it to the configured `api` origin and nowhere else: redirects are not
  followed (`relay.go`, `New`, `CheckRedirect`).
- Your telemetry credentials leaving your network. Telemetry headers are read
  from your file and set on the upstream request only (`relay.go`, `forward`,
  `up.Headers`); they are never printed (`routes.go`, `Routes`) or logged,
  and Perfloop's own headers cannot override them, because yours are set
  last.
- Transport being mistaken for repository permission. Code-host TLS runs
  end to end from the Perfloop proxy to the code host. The relay has no
  code-host credentials, certificate trust, or repository authorization
  logic. It copies opaque bytes, not provider HTTP requests (`connect.go`).

What the relay trusts:

- Perfloop's validation of telemetry reads and the proxy's code-host
  authorization. The relay forwards telemetry query strings unchanged,
  except for pprof's checked `seconds` cap (`relay.go`, `forward`;
  `routes.go`, `pprofQuery`). It cannot inspect encrypted code-host requests.
- TLS to the configured origin: the system roots, or only `api_ca` when set,
  TLS 1.2 or later (`relay.go`, `New`, `TLSClientConfig`). A wrong
  certificate ends the handshake (`relay_test.go`,
  `TestTunnelRefusesUntrustedAPICertificate`).
- Your telemetry upstream not echoing request headers into responses. Responses are
  forwarded as they are (`relay.go`, `forward`, the `io.Copy`).

What stays your job:

- Custody and rotation of the relay token, in the Secret your file
  references, and revoking it in Perfloop Setup when it may have leaked.
- A read-only, query-scoped token for each telemetry upstream in its `headers`.
- Configuring code-host credentials and certificate trust in Perfloop, not
  in the relay. A code-host origin in the relay grants network reachability
  only, not permission to use a repository.
- The NetworkPolicy: egress to the Perfloop API host on 443 and to your
  upstreams, nothing else, and no ingress (`examples/kubernetes/`).
- Choosing when to roll a new image, after reading its commit and verifying
  its signature (`docs/image.md`). Nothing updates the relay for you.

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
  the relay is the HTTP/2 server (`relay.go`, `serve`). Telemetry reads are
  ordinary HTTP GET requests. Code-host connections use HTTP/2 CONNECT.
  The relay never opens a stream of its own.
- The relay keeps two tunnels open so the loss of one never leaves you
  without a tunnel, reopens a tunnel that ends with a backoff of at most ten
  seconds, and pings a tunnel that has been silent for thirty seconds; a ping
  with no answer within ten seconds ends the tunnel. At most eight streams,
  counting both reads and CONNECTs, are in flight on one tunnel; the tunnel's
  HTTP/2 settings enforce it.
- On `SIGINT` or `SIGTERM` the relay closes every tunnel at once and exits
  (`relay.go`, `Run` and `serve`; `cmd/perfloop-relay/main.go`). A read in
  flight on a closed tunnel fails; Perfloop retries a read whose response had
  not started on another live tunnel of the tenant, which is why the relay
  keeps two, and reports the read as unavailable when none is left. There is
  no drain. Code-host connections close with their tunnel. CONNECT bytes
  are never replayed; a new CONNECT is a new TCP connection.

## What the relay forwards, and what it refuses

Every telemetry read is checked in `relay.go`, `forward`, before any upstream request is
made. The route table it checks against is `routes.go`, and it is the same
table that `-print-routes` prints, so the printed listing is what the relay
does.

A telemetry read is forwarded only when all of these hold:

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
| `pprof` | `/<target>/debug/pprof/` | `profile`, `heap`, `allocs`, `goroutine`, `mutex`, `block` |

`{name}` is one non-empty path segment. `<target>` is a name in the pprof
upstream's `targets`; any other answers `404 unknown target`. A pprof read's
query must be empty or exactly one `seconds` from 1 to the upstream's
`max_seconds`, and `profile` must carry it; anything else answers `400`. A
read past the upstream's `max_concurrent` answers `429`. Admin, write, push, and delete routes
are not in the table and are refused before any upstream request; the tests in
`routes_test.go` and `relay_test.go` include the cases `admin/tsdb/delete_series`,
`admin/tsdb/snapshot`, `write`, `push`, and a `..` in the path.

A forwarded request is built as: the upstream `url` plus the read's path,
the read's query string unchanged, and, of the request headers Perfloop
sent, exactly `Accept`, `Accept-Encoding`, `User-Agent`, and `X-Scope-OrgID`
(`link.ForwardedHeaders`; `-print-routes` prints them). Every other request
header, the `Perfloop-Request-Id` included, stops at the relay. Then the
upstream's configured `headers` are set and replace any header of the same
name: a customer who wants to pin the Loki tenant sets `X-Scope-OrgID` in
`headers:`, and the value Perfloop sent never reaches the upstream. The
upstream response's status, headers, and body are returned to Perfloop as
they are, except that hop-by-hop headers and every `Perfloop-*` header the
upstream sent are removed, so an upstream cannot speak as Perfloop. A
redirect from the upstream is not followed: the read fails with `502
upstream request failed`, like any other upstream error.

Bounds on one read: two minutes end to end, and a response body of at most
32 MiB. A larger body aborts the read so that Perfloop sees a broken read,
never a short body that looks complete.

## Code-host CONNECT transport

A `codehost` upstream accepts only HTTP/2 `CONNECT` whose authority equals
its configured name, one lowercase DNS label. A host or port in place of the
name gets `404 unknown upstream`. Other methods get `405`. CONNECT has no
path, query, or scheme pseudo-header; malformed HTTP/2 CONNECTs are rejected
by the HTTP/2 server before the handler. The handler also refuses a path or
query with `400` (`connect.go`, `connect`).

The configuration accepts one HTTPS origin, with no path, query, fragment,
or user info. Its TCP address is derived at startup, with port 443 as the
default (`config.go`, `codehostAddress`; `relay.go`, `New`). `headers`, `ca`,
pprof targets, and pprof caps are not accepted for this kind. There is no
request-supplied address, redirect handling, provider API logic, or generic
TCP destination parameter.

The relay makes one TCP dial, bounded to fifteen seconds, directly to that
address. A dial failure returns `502` with `Perfloop-Relay-Error: relay`.
On success it returns `200` and flushes the headers before waiting for any
TLS bytes. It then copies bytes in both directions and flushes response
chunks, including small TLS handshake records. It does not terminate TLS,
receive plaintext code-host credentials, or add code-host HTTP headers. No
CONNECT request headers are sent to the code host. Certificate validation,
provider credentials, and repository authorization belong to the proxy.

Only code-host streams bypass the telemetry timeout and 32 MiB response
limit. They still share the eight-stream tunnel limit and HTTP/2 flow
control. A request END_STREAM half-closes the TCP upload so the code host
can finish its response. Upstream EOF ends the stream. A reset, closed
tunnel, or stopped relay closes the TCP connection, even if the code host
is silent. An error after `200` aborts the stream instead of returning a
clean short response. The relay never replays stream bytes (`connect.go`).
Token revocation closes these connections when the API ends the tunnels,
under the same revocation bound as telemetry.

## What the relay does not do

- It does not inspect or rewrite upstream responses. There is no scanning
  of response bodies for credentials and no redaction; the body Perfloop
  receives is the body the upstream sent (`relay.go`, `forward`, the
  `io.Copy` after `WriteHeader`). Your protections are the ones you already
  own: give each telemetry upstream a read-only, query-scoped token in its
  `headers`, and use an upstream that does not echo request headers into
  responses. Code-host bytes stay inside end-to-end TLS and are not parsed.
- It has no inbound listener. It does not bind a port. The only network
  connections it opens are the tunnels to the configured `api` and the
  requests to the configured upstreams.
- It takes no configuration push, no remote update, and no command. There is
  no shell, no exec route, and no route that changes its state. A new relay
  version is a new image you choose to roll.
- It does not grant repository permission. The telemetry route table and
  fixed code-host destinations are controls you own. Perfloop's validation
  and code-host authorization happen before the transport is used; the relay
  does not repeat or replace them.
- When the relay token is revoked in Perfloop Setup, the Perfloop API ends
  its open tunnels within 40 seconds (the API checks each tunnel's key on a
  schedule; the relay takes no part in it), and every reconnect after that
  is refused with `401 Unauthorized`, which the relay logs as
  `perfloop refused tunnel`.
  Stopping the relay ends its tunnels at once.

## The relay token

A relay token is a Perfloop API key with the `relay` scope. A tenant admin
creates it in Setup, under Telemetry, Relay tokens, and revokes it there.
The plaintext is shown once; Perfloop stores only its hash. The token lets a
relay answer that tenant's validated reads and configured code-host streams:
it grants transport reachability, not repository permission. The Perfloop API
refuses it as a bearer everywhere but the tunnel route, so a token held in
your network reads no Perfloop data. More than one relay may run with the
same token; the API uses any live tunnel of the tenant and accepts a limited
number of tunnels per tenant, refusing another with status 429 until one
ends.
