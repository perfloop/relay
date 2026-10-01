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

# The sources this relay may read. At least one is required.
upstreams:
  - # The name Setup registers as the endpoint `relay://<name>`. One
    # lowercase DNS label: a-z, 0-9, and -, up to 63 characters, no
    # leading or trailing -. Names must be unique in the file.
    name: vm
    # One of the kinds routes.go lists: today prometheus, victoriametrics,
    # loki, or pprof. Selects the read routes
    # (docs/security.md, "What the relay forwards").
    kind: victoriametrics
    # The provider base: scheme, host, port, and an optional path prefix.
    # No user info, query, fragment, or trailing slash. The read's path is
    # appended to it. Required for every kind but pprof, which names
    # targets instead.
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
```

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
