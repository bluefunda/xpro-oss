# syntax=docker/dockerfile:1

# GO_VERSION is auto-injected by docker-deploy.yml from go.mod, if used.
ARG GO_VERSION=1.26

# ---- build ----
# The builder always runs on the build host's own architecture and cross-compiles
# (pure Go, CGO off), so multi-arch builds need no QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH

# Optional: token for private bluefunda modules (btp-go). Scoped to this
# stage only — never reaches the final image.
ARG GH_PAT

# Use the toolchain in the image; never auto-download another one.
ENV GOTOOLCHAIN=local \
    GOPRIVATE=github.com/bluefunda/* \
    CGO_ENABLED=0

RUN apk add --no-cache git \
 && if [ -n "$GH_PAT" ]; then \
      git config --global url."https://${GH_PAT}@github.com/".insteadOf "https://github.com/"; \
    fi

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /xpro-oss . \
 && GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /migrate ./cmd/migrate

# ---- runtime ----
# distroless/static ships CA certificates, tzdata and a nonroot user (65532),
# which is all a static, CGO-free Go binary needs — no shell, no package manager.
FROM gcr.io/distroless/static:nonroot

LABEL org.opencontainers.image.title="xpro-oss" \
      org.opencontainers.image.description="Config-driven XML ingestion daemon: files in, PostgreSQL / Oracle / SAP HANA out" \
      org.opencontainers.image.source="https://github.com/bluefunda/xpro-oss" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="bluefunda"

COPY --from=builder /xpro-oss /xpro-oss
# Schema helper: run with --entrypoint /migrate <schema.sql> (reads /config.yaml).
COPY --from=builder /migrate /migrate
COPY config.yaml /config.yaml
# License texts travel with the binary (Apache-2.0 / BSD / MIT / LGPL notices).
COPY LICENSE THIRD_PARTY_LICENSES /licenses/
COPY licenses/xmlpath-LICENSE /licenses/xmlpath-LICENSE

EXPOSE 8080
# distroless/static:nonroot starts in /home/nonroot; xpro-oss and /migrate read
# ./config.yaml, so run from / where config.yaml lives (mount yours over it).
WORKDIR /
USER nonroot:nonroot
ENTRYPOINT ["/xpro-oss"]
