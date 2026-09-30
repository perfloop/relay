# Setting up the relay

An ordered procedure with a checkpoint after each step. Written so that a
person or an agent can run it start to finish without guessing. Every command
is complete; the only inputs are the ones in the first section.

## Inputs

Collect these before starting. Nothing else is needed.

| Input | Where it comes from |
| --- | --- |
| `<tenant>` | Your Perfloop tenant name, the segment after `/t/` in the app URL. |
| `<namespace>` | The Kubernetes namespace the relay will run in. |
| `<upstream-name>` | A name you choose for the source, one lowercase DNS label, for example `vm`. |
| `<kind>` | One of the kinds `routes.go` lists: today `prometheus`, `victoriametrics`, or `loki`. |
| `<upstream-url>` | The provider base URL as reachable from inside the cluster, for example `http://vmselect.monitoring.svc:8481/select/0/prometheus`. No trailing slash. |
| `<upstream-read-token>` | A read-only token for that upstream, if it needs one. Query-scoped. |
| `<digest>` | The relay image digest to pin. See "Choose the image" below. |
| A Perfloop admin | Creating the relay token and registering the source happen in Perfloop Setup, which needs an admin login in the browser. |

Access needed: `kubectl` against the cluster with rights to create a
ConfigMap, Secret, Deployment, and NetworkPolicy in `<namespace>`; outbound
HTTPS from that namespace to `app.perfloop.ai` on port 443; and network
reachability from the namespace to the upstream.

## 1. Choose the image

Every push to `main` in this repository publishes
`ghcr.io/perfloop/relay` tagged with the commit SHA. Pick the commit you
read, and resolve its digest:

```sh
docker buildx imagetools inspect ghcr.io/perfloop/relay:<commit sha> --format '{{json .Manifest.Digest}}'
```

Checkpoint: the command prints one `sha256:…` string. That is `<digest>`.
To verify the signature and the binary, see [image.md](image.md).

## 2. Create the relay token

In the Perfloop app, open `https://app.perfloop.ai/t/<tenant>/setup`, and in
**Model inputs**, at the end of the **Telemetry** section, find
**Relay tokens**. Enter a name for the token (for example `relay <namespace>`)
and press **Create relay token**. Copy the token now; it is shown once. It
starts with `plf_`.

Checkpoint: the token's name appears in the Relay tokens list.

Hold the token in an environment variable in your shell, never in a file
or on a command line that shell history records:

```sh
read -rs PERFLOOP_RELAY_TOKEN   # paste, then Enter
```

## 3. Write the configuration and validate it before deploying

Write `relay.yaml`:

```yaml
api: https://app.perfloop.ai
token: ${PERFLOOP_RELAY_TOKEN}
upstreams:
  - name: <upstream-name>
    kind: <kind>
    url: <upstream-url>
    headers:
      Authorization: Bearer ${UPSTREAM_READ_TOKEN}
```

Drop the `headers` block if the upstream needs no token. Every field is
described in [configuration.md](configuration.md).

Validate it and print exactly what it permits, using the same image you will
deploy. `-print-routes` only checks the file; placeholders are fine for the
secrets here:

```sh
docker run --rm -v "$PWD/relay.yaml:/etc/perfloop-relay/relay.yaml:ro" \
  -e PERFLOOP_RELAY_TOKEN=placeholder -e UPSTREAM_READ_TOKEN=placeholder \
  ghcr.io/perfloop/relay@<digest> -print-routes
```

Checkpoint: exit code 0 and a listing whose `forwards` URLs are the ones you
expect. Any other exit code is a configuration error, printed on the last
line. Keep the listing with your change record: it is everything the relay
can be asked to do.

## 4. Create the Secret

From the variables, not from literals:

```sh
read -rs UPSTREAM_READ_TOKEN   # paste, then Enter (skip if the upstream needs none)
kubectl -n <namespace> create secret generic perfloop-relay \
  --from-literal=token="$PERFLOOP_RELAY_TOKEN" \
  --from-literal=upstream-read-token="${UPSTREAM_READ_TOKEN:-}"
unset PERFLOOP_RELAY_TOKEN UPSTREAM_READ_TOKEN
```

Checkpoint: `kubectl -n <namespace> get secret perfloop-relay` lists it with
two keys.

## 5. Deploy

Take [examples/kubernetes/relay.yaml](../examples/kubernetes/relay.yaml),
delete its `Secret` document (you created the real one in step 4), replace
every `<placeholder>` including `<digest>`, put your `relay.yaml` content in
the ConfigMap, and apply:

```sh
kubectl -n <namespace> apply -f relay.yaml
kubectl -n <namespace> rollout status deploy/perfloop-relay --timeout=120s
```

Checkpoint: the rollout completes, and within ten seconds the log shows two
open tunnels:

```sh
kubectl -n <namespace> logs deploy/perfloop-relay | grep '"tunnel open"' | head -2
```

Two lines with `"msg":"tunnel open","api":"app.perfloop.ai"`.

## 6. Register the source in Perfloop

In `https://app.perfloop.ai/t/<tenant>/setup`, in the **Model inputs** group,
press **Connect <Provider>** for your kind (Prometheus, VictoriaMetrics, or
Loki). In the **Endpoint** field enter:

```
relay://<upstream-name>
```

Leave the credential empty; a relay source has none. For Loki, enter the
tenant id if your Loki is multi-tenant. Complete the connection.

Checkpoint: Setup shows the source as connected, and the relay log shows the
connection check as a read:

```sh
kubectl -n <namespace> logs deploy/perfloop-relay | grep '"relay read"' | tail -1
```

One line with `"status":200`.

## 7. Confirm a real read

The first Perfloop Session or research that uses this source produces reads
with a request id:

```sh
kubectl -n <namespace> logs deploy/perfloop-relay | grep '"request_id":"[^"]' | tail -3
```

Each line's `request_id` is `<session id>/<tool call ref>`; it names the
Perfloop transcript event that asked. That is your join key between this log
and Perfloop.

## If a checkpoint fails

| What you see | Meaning | What to check |
| --- | --- | --- |
| `-print-routes` exits non-zero | The file is invalid. | The last line names the field and the rule. |
| No `tunnel open`; `tunnel ended` with a dial or TLS error | The relay cannot reach `app.perfloop.ai:443`. | Egress from the namespace, the NetworkPolicy, DNS, an egress proxy (`HTTPS_PROXY` must carry WebSockets), `api_ca` if TLS is re-signed. |
| `perfloop refused tunnel: 401 Unauthorized` | The token is wrong or revoked. | Create a new token in Setup and replace the Secret's `token` key; the Deployment restarts on its own only if you roll it. |
| `perfloop refused tunnel: 429` | This tenant already has the maximum tunnels on that API replica. | Another relay of yours is connected; stop the extra one or wait for it to end. |
| Setup connection fails; relay log shows `"decision":"upstream-error"` | The relay reached the tunnel but not the upstream. | `url`, the upstream's TLS (`ca`), and network reachability from the namespace to the upstream. |
| Setup connection fails; relay log shows `"status":404,"decision":"unknown-upstream"` | The endpoint name in Setup does not match `upstreams[].name`. | Use exactly `relay://<upstream-name>`. |
| Setup connection fails; relay log shows no line at all | The read never reached this relay. | Confirm the tunnels are open; the tenant in Setup is the tenant the token belongs to. |
| `"decision":"truncated"` | An upstream response exceeded 32 MiB and was cut. | Narrow the query on the Perfloop side; the relay does not buffer. |

## Rotating the token

Create the new token in Setup, update the Secret's `token` key, roll the
Deployment, confirm two `tunnel open` lines, then revoke the old token in
Setup. Tunnels on the old token end within thirty seconds of revocation, so
do it in that order to avoid a gap.
