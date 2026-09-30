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
    # prometheus, victoriametrics, or loki. Selects the read routes
    # (docs/security.md, "What the relay forwards").
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
