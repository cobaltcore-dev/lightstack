# Multi-stage build for Lightstack - Multi-architecture support
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

# Build arguments for cross-compilation
ARG TARGETOS
ARG TARGETARCH

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
    go build -ldflags="-w -s" -o lightstack ./cmd/lightstack

# Build the migration tool for target architecture
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-w -s" -o lightstack-migrate ./cmd/lightstack-migrate

# Runtime stage - automatically uses target platform
FROM alpine:3.24

# Install runtime dependencies.
# qemu-img: per-instance qcow2 overlay staging (WS-0); cdrkit: genisoimage for
# config-drive cloud-init ISOs (WS-4); dnsmasq: VM DHCP; iproute2/iptables/ipset:
# real-mode networking.
RUN apk add --no-cache \
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

# Run migrations and start Lightstack
CMD ["/bin/sh", "-c", "/app/lightstack-migrate up && /app/lightstack --config /app/config/lightstack.yaml"]
