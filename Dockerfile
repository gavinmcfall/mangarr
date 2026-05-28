# syntax=docker/dockerfile:1

# ── builder ─────────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Download modules first so they are cached when only source changes.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a fully static binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/mangarr \
      .

# ── runtime ─────────────────────────────────────────────────────────────────
FROM alpine:3.21 AS runtime

# OCI image labels (T11 CI will override VCS/version-specific ones at push time).
LABEL org.opencontainers.image.title="mangarr" \
      org.opencontainers.image.description="Manga download watcher and organiser" \
      org.opencontainers.image.source="https://github.com/gavinmcfall/mangarr" \
      org.opencontainers.image.licenses="MIT"

# Non-root user — uid 568 matches home-ops k8s convention.
RUN addgroup -g 568 -S mangarr && \
    adduser  -u 568 -S mangarr -G mangarr

# Expected mount-points.
RUN mkdir -p /config /media && \
    chown 568:568 /config /media

COPY --from=builder /out/mangarr /usr/local/bin/mangarr

# Config lives in /config; media in /media.
VOLUME ["/config", "/media"]

EXPOSE 8590

USER 568:568

ENTRYPOINT ["/usr/local/bin/mangarr"]
