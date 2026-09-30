# Security model

What runs in your network, what it can be asked to do, and what it cannot. Every claim names the code that enforces it.

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
