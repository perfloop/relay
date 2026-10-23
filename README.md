# Perfloop relay

A small program you run inside your network so Perfloop can read the
telemetry you keep there: metrics, logs, profiles, and traces from providers the
public internet cannot reach. It also carries end-to-end TLS to self-managed
GitLab and GitHub Enterprise Server. This repository is the complete source
of what runs there. The image Perfloop publishes is built from it, by the
workflow in this repository, and is tagged with the commit it was built from.

Sources the relay reads today: Prometheus, VictoriaMetrics, Loki, and Go
pprof. Each telemetry kind is one table of read routes in `routes.go`.
The `codehost` kind carries opaque CONNECT streams to one configured HTTPS
origin. A kind the relay does not know is refused when it starts.

1084 lines of Go outside tests, three runtime dependencies (`coder/websocket`,
`go.yaml.in/yaml/v3`, `golang.org/x/net`): read it in an afternoon.

## What it does

- Opens outbound tunnels to the Perfloop API. It binds no port.
- Answers reads that Perfloop already validated by forwarding each one to an
  upstream you named in its configuration file, and nothing else.
- For telemetry, forwards only `GET` requests to documented read routes.
  Admin, write, push, and delete routes are refused before any upstream request.
- For code hosts, accepts HTTP/2 `CONNECT` to a fixed upstream name and
  streams bytes only to its configured origin. The proxy owns TLS,
  credentials, and repository authorization. Transport grants reachability,
  never repository permission.
- Logs one line per read or completed CONNECT, with a request id you can join
  to the Perfloop session that asked.

## What it does not do

- Telemetry credentials stay in your network. Upstream URLs and headers are
  read from the relay's own file and sent only to that upstream. Code-host
  credentials stay at the Perfloop proxy and cross only inside end-to-end TLS;
  the relay does not receive them in plaintext or add provider headers.
- Perfloop cannot push configuration or commands. The file is read once, at
  start. There is no shell, no exec route, and no remote update.
- It does not inspect or rewrite upstream responses. Give telemetry upstreams
  read-only tokens. Code-host streams do not use the telemetry GET route table
  or response limits; the relay cannot inspect their encrypted requests.
- Revoking the relay token in Perfloop Setup ends its tunnels within 40
  seconds, enforced by the Perfloop API. Stopping the relay ends its tunnels
  at once.

The full model, with the code that enforces each point:
[docs/security.md](docs/security.md).

## Telemetry quick start

1. In Perfloop Setup, under Telemetry, Relay tokens, create a relay token. It is shown once.
2. Write `relay.yaml`:

   ```yaml
   api: https://app.perfloop.ai
   token: ${PERFLOOP_RELAY_TOKEN}
   upstreams:
     - name: vm
       kind: victoriametrics
       url: http://vmselect.monitoring.svc:8481/select/0/prometheus
       headers:
         Authorization: Bearer ${VM_READ_TOKEN}
   ```

3. See exactly what this file lets the relay do, before running it:

   ```sh
   perfloop-relay -config relay.yaml -print-routes
   ```

4. Run it, in your cluster, with outbound access to the Perfloop API host on
   443 and to the upstreams:

   ```sh
   perfloop-relay -config relay.yaml
   ```

   For Kubernetes, start from [examples/kubernetes/](examples/kubernetes/):
   a hardened Deployment and a NetworkPolicy that allows egress only to the
   Perfloop API and your upstreams.

5. In Perfloop Setup, register the source as `relay://vm`.

The same steps as an ordered procedure with a checkpoint after each one, and
a table of what each failure means: [docs/setup.md](docs/setup.md). Agents
start there ([AGENTS.md](AGENTS.md)).

Every configuration field: [docs/configuration.md](docs/configuration.md).
Running, `-print-routes`, and the audit log: [docs/operations.md](docs/operations.md).

## Self-managed code hosts

Add one fixed HTTPS origin for each code host the relay may reach:

```yaml
upstreams:
  - name: git
    kind: codehost
    url: https://git.example.com
```

Do not add `headers`, `ca`, pprof options, or targets to this upstream. Configure
the code-host credentials, trust, and repository authorization in Perfloop,
not in the relay. The CONNECT authority is `git`, with no path or query;
the relay dials only `git.example.com:443`. An explicit port is also allowed.
See [the configuration contract](docs/configuration.md#code-host-upstreams)
and [the security model](docs/security.md#code-host-connect-transport).

## The image

```
ghcr.io/perfloop/relay:<commit sha>
```

Pin it by digest. The binary is reproducible from the commit, and each
published digest is signed with keyless cosign; how to verify both:
[docs/image.md](docs/image.md).

Vulnerabilities: [SECURITY.md](SECURITY.md).

## Development

`go test -race ./...`, `go vet ./...`, `golangci-lint run ./...`. What the
tests prove: [docs/development.md](docs/development.md).

## License

Apache License 2.0. See `LICENSE` and `NOTICE`.
