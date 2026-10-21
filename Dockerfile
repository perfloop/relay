# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

# The relay image: one static binary on a distroless base, run as nonroot.
# The build reads only this module (.dockerignore keeps everything else out).
# The binary is reproducible from the commit (pinned toolchain, go.sum,
# -trimpath); the image digest is not, so README.md tells a customer how to
# compare the binary.
FROM golang:1.27.3-bookworm@sha256:21ade014faf952365b9ecbd8dbdb87176fbacb82fac9fb132d9f2a7b8cc3f132 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/perfloop-relay ./cmd/perfloop-relay

FROM gcr.io/distroless/static-debian13:nonroot@sha256:516757106de35205efb70b91ba03f6f1298424a0678c603255cf2852a7000087
COPY --from=build /out/perfloop-relay /usr/local/bin/perfloop-relay
USER nonroot
ENTRYPOINT ["/usr/local/bin/perfloop-relay"]
