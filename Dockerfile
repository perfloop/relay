# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

# The relay image: one static binary on a distroless base, run as nonroot.
# The build reads only this module (.dockerignore keeps everything else out).
# The binary is reproducible from the commit (pinned toolchain, go.sum,
# -trimpath); the image digest is not, so README.md tells a customer how to
# compare the binary.
FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/perfloop-relay ./cmd/perfloop-relay

FROM gcr.io/distroless/static-debian13:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6
COPY --from=build /out/perfloop-relay /usr/local/bin/perfloop-relay
USER nonroot
ENTRYPOINT ["/usr/local/bin/perfloop-relay"]
