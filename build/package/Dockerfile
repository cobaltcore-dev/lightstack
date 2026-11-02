# Multi-stage build for Lightstack - Multi-architecture support
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine3.24 AS builder

# Build arguments for cross-compilation
ARG TARGETOS
ARG TARGETARCH
# Version stamp for the binary. Declared HERE, in the builder stage: an ARG
# only visible in a later stage is invisible to this stage's RUN, and Docker
# silently expands it to empty — the image would then carry an unidentifiable
# binary (-X main.Version= with an empty value), the exact failure R1 fixed.
ARG VERSION=dev

# Install build dependencies
RUN apk add --no-cache git make

# Set working directory
WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary for target architecture
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-w -s -X main.Version=${VERSION}" -o lightstack ./cmd/lightstack

# Build the migration tool for target architecture
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-w -s" -o lightstack-migrate ./cmd/lightstack-migrate

# Runtime stage - automatically uses target platform
FROM alpine:3.24

# Install runtime dependencies.
# qemu-img: per-instance qcow2 overlay staging (WS-0); cdrkit: genisoimage for
# config-drive cloud-init ISOs (WS-4); dnsmasq: VM DHCP; iproute2/iptables/ipset:
# real-mode networking.
#
# The base image's own packages are upgraded first: `apk add` leaves anything
# already installed at the version the base tag was cut with, so a security fix
# released since (zlib, OpenSSL) would never reach the image.
RUN apk upgrade --no-cache && \
    apk add --no-cache \
    ca-certificates \
    curl \
    tzdata \
    iproute2 \
    iptables \
    ipset \
    dnsmasq \
    qemu-img \
    cdrkit

# Create non-root user (but will run as root in privileged mode for networking)
RUN addgroup -g 1000 lightstack && \
    adduser -D -u 1000 -G lightstack lightstack

# Set working directory
WORKDIR /app

# Copy binaries from builder
COPY --from=builder /build/lightstack /app/lightstack
COPY --from=builder /build/lightstack-migrate /app/lightstack-migrate

# Copy migrations
COPY migrations /app/migrations

# Copy default config
COPY config/lightstack.yaml /app/config/lightstack.yaml

# Create node-local storage directories (persistent hostPath in real mode):
# images (raw bases), volumes (local volumes), instances (per-instance overlays),
# cloud-init (config-drive ISOs).
RUN mkdir -p /var/lib/lightstack/images /var/lib/lightstack/volumes /var/lib/lightstack/instances /var/lib/lightstack/cloud-init && \
    chown -R lightstack:lightstack /var/lib/lightstack

# Change ownership
RUN chown -R lightstack:lightstack /app

# Note: Running as root for network namespace operations in privileged mode
# The container must be run with privileged: true for real networking mode

# Expose ports
EXPOSE 35357 8774 9696 8776 9292 8775

# Health check (use internal port 35357)
HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=3 \
    CMD curl -f http://localhost:35357/v3 || exit 1

ARG VCS_REF=unknown
ARG SOURCE_URL=https://github.com/cobaltcore-dev/lightstack
ARG VERSION=dev

LABEL org.opencontainers.image.title="Lightstack" \
    org.opencontainers.image.description="Lightweight OpenStack-compatible API service" \
    org.opencontainers.image.source=$SOURCE_URL \
    org.opencontainers.image.revision=$VCS_REF \
    org.opencontainers.image.version=$VERSION

# Run migrations and start Lightstack.
#
# Note on the secret guard (S2): the server refuses the canned config's
# well-known defaults — both the JWT secret and the bootstrap admin password
# ("admin"/"secret", which is public and authenticates as the admin directly).
# A bare `docker run` of this image therefore needs LIGHTSTACK_JWT_SECRET and
# LIGHTSTACK_ADMIN_PASSWORD set (or LIGHTSTACK_ALLOW_INSECURE_DEFAULT_SECRET=1
# for an isolated dev container). lightstack-migrate above is exempt — it
# never signs or validates a token.
CMD ["/bin/sh", "-c", "/app/lightstack-migrate up && /app/lightstack --config /app/config/lightstack.yaml"]
