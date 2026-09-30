# Running the relay

```sh
perfloop-relay -config /etc/perfloop-relay/relay.yaml
```

`-config` defaults to `/etc/perfloop-relay/relay.yaml`. Logs are JSON lines
on standard output.

## `-print-routes`

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

## The audit log

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
