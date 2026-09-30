# Perfloop relay

A small program you run inside your network so Perfloop can read the
telemetry you keep there: metrics, logs, profiles, and traces from providers the
public internet cannot reach. This repository is the complete source of what
runs there. The image Perfloop publishes is built from it, by the workflow in
this repository, and is tagged with the commit it was built from.

Sources the relay reads today: Prometheus, VictoriaMetrics, and Loki. Go
pprof and Pyroscope are next. Each kind is one table of read routes in
`routes.go`, and a kind the relay does not know is refused when it starts.

756 lines of Go outside tests, three runtime dependencies (`coder/websocket`,
`go.yaml.in/yaml/v3`, `golang.org/x/net`): read it in an afternoon.

## What it does

- Opens outbound tunnels to the Perfloop API. It binds no port.
- Answers reads that Perfloop already validated by forwarding each one to an
  upstream you named in its configuration file, and nothing else.
- Forwards only `GET` requests to documented read routes. Admin, write, push,
  and delete routes are refused before any upstream request.
- Logs one line per read, with a request id you can join to the Perfloop
  session that asked.

## What it does not do

- No credential leaves your network. Upstream URLs and headers are read from
  the relay's own file and sent only to that upstream.
- Perfloop cannot push configuration or commands. The file is read once, at
  start. There is no shell, no exec route, and no remote update.
- It does not inspect or rewrite upstream responses. Give each upstream a
  read-only token.
- Revoking the relay token in Perfloop Setup ends its tunnels within thirty
  seconds, enforced by the Perfloop API.

The full model, with the code that enforces each point:
[docs/security.md](docs/security.md).

## Quick start

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
