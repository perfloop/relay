# The image, and how to verify it

The image is published by `.github/workflows/image.yml` on every push to
`main`, at

```
us-central1-docker.pkg.dev/perfloop-public-prod/perfloop-images/relay
```

tagged with the full and the short SHA of the commit it was built from, so a
tag names a commit of this repository. Pin the image by digest in your
manifests. To verify that the image is what this repository builds:

1. Open the `Image` workflow run for that commit under Actions in this
   repository. Its summary lists the tags and the manifest digest.
2. Resolve the tag yourself and compare the digest:

   ```sh
   docker buildx imagetools inspect us-central1-docker.pkg.dev/perfloop-public-prod/perfloop-images/relay:<commit sha>
   ```

3. Compare the binary, not the image. The binary is reproducible: the Go
   toolchain is pinned by digest in the `Dockerfile`, the dependencies by
   `go.sum`, and the build uses `-trimpath -buildvcs=false -ldflags="-s -w"`,
   so the same commit gives the same bytes. The image digest is not
   reproducible (layer metadata and the SBOM attestation differ between
   builds), so a rebuilt image will not match the published digest. From the
   commit's checkout:

   ```sh
   image=us-central1-docker.pkg.dev/perfloop-public-prod/perfloop-images/relay@<digest>
   id="$(docker create "$image")"
   docker cp "$id:/usr/local/bin/perfloop-relay" published-relay
   docker rm "$id" >/dev/null
   docker run --rm -v "$PWD:/src" -w /src \
     golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 \
     sh -c 'CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /src/local-relay ./cmd/perfloop-relay'
   sha256sum published-relay local-relay
   ```

   The two sums are equal.

## The signature

Every published digest is signed with keyless [cosign](https://docs.sigstore.dev)
by the `Image` workflow itself: the signing identity is the workflow's own
GitHub OIDC token, the certificate is issued by Fulcio and recorded in the
Rekor transparency log, and no signing key exists anywhere. Verify that the
image you pull was published by this repository's `main` branch:

```sh
cosign verify \
  --certificate-identity https://github.com/perfloop/relay/.github/workflows/image.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  us-central1-docker.pkg.dev/perfloop-public-prod/perfloop-images/relay@<digest>
```

A signature by any other repository, branch, or workflow fails this check.
Signing starts with the first publish after the signing step landed in
`image.yml`; images published before it carry no signature.

The image runs the binary as `nonroot` on a distroless base with nothing
else in it. A published image carries an SPDX SBOM attestation.

A Kubernetes Deployment needs: the configuration file in a ConfigMap mounted
at `/etc/perfloop-relay/relay.yaml`, the token in a Secret exposed as the
environment variable the file references, and outbound access to the Perfloop
API host on 443 and to the upstreams. Nothing needs to reach the relay.
