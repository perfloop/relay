# Running the relay

```sh
perfloop-relay -config /etc/perfloop-relay/relay.yaml
```

`-config` defaults to `/etc/perfloop-relay/relay.yaml`. Logs are JSON lines
on standard output.

A Kubernetes starting point, with a hardened pod and a NetworkPolicy that
allows only the egress the relay needs, is in `examples/kubernetes/`. The
ordered setup procedure with checkpoints is [setup.md](setup.md).

## `-print-routes`

```sh
perfloop-relay -config relay.yaml -print-routes
```

loads and validates the configuration and prints, per upstream, its kind,
its URL (for `pprof`, its targets and caps), the request headers it passes from Perfloop, the names of its
configured headers, and every request the relay will forward, then exits.
For `codehost`, it prints the fixed CONNECT name and TCP address, that the
bytes are opaque, and that TLS, credentials, and repository authorization
stay at the proxy. An invalid configuration exits non-zero with the
validation error. Header values and the token are not printed. Example:

```
upstream vm
  kind    victoriametrics
  url     http://vm.internal:8428/select/0/prometheus
  passes  Accept, Accept-Encoding, User-Agent, X-Scope-OrgID from Perfloop; every other request header is dropped
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

## The audit log

The relay writes one line per read or CONNECT, `msg: "relay read"`, when it
ends, with these fields:

| Field | Meaning |
| --- | --- |
| `upstream` | The upstream name the read asked for. |
| `method`, `path` | The method and path as Perfloop sent them. CONNECT has an empty path. The query string is not logged, in this field or in `error`. |
| `request_id` | The Perfloop work behind the read, `<session id>/<tool call ref>`, from the `Perfloop-Request-Id` header. Empty for a read no tool call made, such as a connect check from Setup. |
| `status` | The status the relay answered. |
| `decision` | The read route that was forwarded (`query`, `labels`, ...), `connect` for a completed code-host stream, or why it failed: `unknown-upstream`, `method`, `path`, `route`, `upstream-error`, or `truncated`; for `pprof` also `unknown-target`, `query`, or `busy` (the `max_concurrent` cap). `truncated` also marks a CONNECT that failed after `200`. |
| `ms` | Time spent on the read or CONNECT stream. |
| `error` | The upstream error, if any, without the request URL (`relay.go`, `forward`). |

`request_id` lets you join a line in your log to the session transcript in
Perfloop that asked for the read. The relay logs it and removes it before the
upstream request, so your provider never sees a Perfloop identifier. Tunnel
life is logged as `tunnel open` and `tunnel ended`, with the error and the
retry delay.
