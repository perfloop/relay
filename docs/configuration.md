# Configuration

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

# The sources this relay may reach. At least one is required.
upstreams:
  - # The name Setup registers as the endpoint `relay://<name>`. One
    # lowercase DNS label: a-z, 0-9, and -, up to 63 characters, no
    # leading or trailing -. Names must be unique in the file.
    name: vm
    # One of the kinds routes.go lists: prometheus, victoriametrics,
    # loki, pprof, or codehost. Selects the transport or read routes
    # (docs/security.md, "What the relay forwards").
    kind: victoriametrics
    # The provider base: scheme, host, port, and an optional path prefix.
    # No user info, query, fragment, or trailing slash. A telemetry read's
    # path is appended to it. A codehost URL must be an HTTPS origin with
    # no path. Required for every kind but pprof, which names targets instead.
    url: http://vmselect.monitoring.svc:8481/select/0/prometheus
    # Optional, telemetry only. Headers added to every request to this
    # upstream; they replace any header of the same name that Perfloop
    # sent. Values are never logged or printed. Refused for codehost.
    headers:
      Authorization: Bearer ${VM_READ_TOKEN}
    # Optional, telemetry only. A PEM file trusted for this upstream's TLS
    # certificate instead of the system roots. Refused for codehost.
    ca: /etc/perfloop-relay/vm-ca.pem
  - name: logs
    kind: loki
    url: https://loki-gateway.monitoring.svc
  - name: go
    kind: pprof
    # Required for kind pprof, at least one. The Go processes this upstream
    # may profile: target name to the http(s) base of that process's
    # net/http/pprof handler, with the same URL rules as `url`. Each name is
    # one lowercase DNS label. Perfloop names one target per read; a name not
    # listed here is refused.
    targets:
      api-1: http://api-1.prod.svc:6060
      api-2: http://api-2.prod.svc:6060/internal
    # Optional, kind pprof only. The longest `seconds` a read may ask for,
    # 1 to 45. Default 30, net/http/pprof's own CPU profile length.
    max_seconds: 10
    # Optional, kind pprof only. Profile reads in flight on this upstream,
    # across all of its targets, per relay process; 1 to 8. Default 1. A
    # further read is refused with status 429.
    max_concurrent: 2
  - name: git
    kind: codehost
    # Only this origin's TCP address can be reached. The default port is 443.
    url: https://git.example.com
```

## Code-host upstreams

A `codehost` upstream carries end-to-end TLS between the Perfloop proxy and
a self-managed GitLab or GitHub Enterprise Server. Its `url` is one HTTPS
origin, with an optional port and no path, query, fragment, or user info.
The relay dials only that origin's TCP address, on port 443 when no port is
set. It does not use `HTTPS_PROXY` for this connection.

Perfloop opens an HTTP/2 `CONNECT` stream inside the existing WebSocket
tunnel. Its authority is exactly the configured upstream `name`, not a host
or port. It has no path or query. After the TCP dial succeeds, the relay
returns `200` and flushes its headers before it reads any stream bytes.
The request and response bodies then carry opaque bytes in both directions.
The relay flushes response bytes as they arrive, including TLS handshake
records. It makes one connection attempt per stream and never replays bytes.

Do not set `headers`, `ca`, `targets`, `max_seconds`, or `max_concurrent`
on a `codehost` upstream. These fields are refused. The relay does not
terminate the code host's TLS or add HTTP headers. Code-host credentials,
certificate trust, and repository authorization belong to the Perfloop proxy,
not to this transport. A relay token grants no repository permission.

Code-host streams share the tunnel's eight-stream limit. They do not use the
telemetry GET timeout or 32 MiB response limit. Closing a stream or its
tunnel, stopping the relay, or revoking its token ends its connection.
`-print-routes` lists the fixed CONNECT name, origin, and transport behavior.

## pprof upstreams

A `pprof` upstream reads `net/http/pprof` from Go processes you name. It has
`targets` instead of `url`; `targets`, `max_seconds`, and `max_concurrent`
are refused on every other kind. The relay forwards `GET` to exactly these
routes under each target's base: `/debug/pprof/profile`, `heap`, `allocs`,
`goroutine`, `mutex`, and `block`. `seconds` is the only query key the relay
accepts: the capture length for `profile`, which must carry it, and an
optional delta window for the other routes. It is one integer from 1 to
`max_seconds`. Every other key, `debug` and `gc` included, is refused, so only
the binary profile leaves the process. `-print-routes` prints the targets and
both caps.

The validation rules are in `config.go`, `Validate`; `config_test.go` lists
every refused shape. The pprof query rules are in `routes.go`, `pprofQuery`.
