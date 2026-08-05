.PHONY: build run test chart-test clean install-deps install-tools migrate db-up db-down build-ebpf bench bench-quick horizon-chart-sync horizon-chart-check test-horizon-ui

# Build variables
BINARY_NAME=o3k
BUILD_DIR=bin
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-X main.Version=$(VERSION)"

# Database variables
POSTGRES_VERSION?=18
# Development-only defaults. Do NOT use in production.
# Production should set DB_URL via environment variable with sslmode=require.
DB_URL?=postgres://o3k:secret@localhost:5432/o3k?sslmode=disable

# Build the binary
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/o3k

# Build eBPF programs
build-ebpf:
	@echo "Building eBPF programs..."
	@which clang > /dev/null || (echo "ERROR: clang not found. Install with: apt-get install clang llvm libbpf-dev" && exit 1)
	clang -O2 -target bpf -c pkg/networking/ebpf/secgroup.c -o pkg/networking/ebpf/secgroup.o
	@echo "eBPF program compiled: pkg/networking/ebpf/secgroup.o"

# Build with eBPF support
build-with-ebpf: build-ebpf build
	@echo "Built O3K with eBPF support"

# Run the application
run: build
	@echo "Starting O3K..."
	./$(BUILD_DIR)/$(BINARY_NAME) --config config/o3k.yaml --migrations migrations

# Install dependencies
install-deps:
	@echo "Installing dependencies..."
	go mod download
	go mod tidy

# Run tests
# -count=1 disables Go's test cache. Without it a run that once passed keeps reporting
# "ok (cached)" after the environment changes — TestMain's os.Getenv does not re-execute
# on a cache hit — so this gate could exit 0 on code that crashes. Kept above the recipe
# so make does not echo it on every run.
test:
	@echo "Running unit tests (excludes test/contract — use 'make test-contract' for those)..."
	go test -count=1 -v $$(go list ./... | grep -v '/test/contract')

# Run contract tests (requires O3K to be running)
test-contract:
	@echo "Running contract tests..."
	@echo "Checking O3K is running..."
	@curl -s http://localhost:35357/v3 > /dev/null || (echo "ERROR: O3K not running. Run 'docker compose -f deployments/docker-compose.yml up -d' or 'make run' first." && exit 1)
	@echo "Running gophercloud contract tests..."
	go test -v ./test/contract/... -timeout 5m

# Run integration tests (requires O3K to be running)
test-integration:
	@echo "Running integration tests..."
	@echo "Checking O3K is running..."
	@curl -s http://localhost:35357/v3 > /dev/null || (echo "ERROR: O3K not running. Run 'docker compose -f deployments/docker-compose.yml up -d' first." && exit 1)
	@echo "Running integration test suite..."
	@bash test/integration_test.sh

# Run multi-tenancy tests
test-multi-tenant:
	@echo "Running multi-tenancy tests..."
	@echo "Checking O3K is running..."
	@curl -s http://localhost:35357/v3 > /dev/null || (echo "ERROR: O3K not running." && exit 1)
	@bash test/multi_tenant_test.sh

# Run performance tests
test-performance:
	@echo "Running performance tests..."
	@echo "Checking O3K is running..."
	@curl -s http://localhost:35357/v3 > /dev/null || (echo "ERROR: O3K not running." && exit 1)
	@bash test/performance_test.sh

# Run the Horizon create/write UI regression probe (SPEC-020, cloud-storage#533)
# Checks the API surface Horizon's create flows load before enabling a control. Retarget
# with KEYSTONE_URL / NOVA_URL / NEUTRON_URL / CINDER_URL; the plain compose stack is
# enough since these are API checks, no browser needed.
test-horizon-ui:
	@echo "Running Horizon create-UI regression probe..."
	@echo "Checking O3K is running..."
	@curl -s http://localhost:35357/v3 > /dev/null || (echo "ERROR: O3K not running. Run 'docker compose -f deployments/docker-compose.yml up -d' first." && exit 1)
	@bash test/horizon_create_ui_test.sh

# Run error handling tests
test-errors:
	@echo "Running error handling tests..."
	@echo "Checking O3K is running..."
	@curl -s http://localhost:35357/v3 > /dev/null || (echo "ERROR: O3K not running." && exit 1)
	@bash test/error_handling_test.sh

# Run Helm chart tests (helm lint + template + unittest; no cluster or running O3K needed)
chart-test:
	@echo "Running Helm chart tests (lint + template + unittest)..."
	@command -v helm >/dev/null 2>&1 || (echo "ERROR: helm not found. Run 'make install-tools' first." && exit 1)
	@helm plugin list 2>/dev/null | grep -q '^unittest' || (echo "ERROR: helm-unittest plugin not found. Run 'make install-tools' first." && exit 1)
	@bash charts/o3k/ci/run-tests.sh
	@$(MAKE) --no-print-directory horizon-chart-check
	@echo "Linting + rendering verification Horizon chart (deployments/horizon-chart)..."
	@helm lint deployments/horizon-chart
	@helm template verify deployments/horizon-chart >/dev/null

# Where horizon-chart-sync writes. Overridden by horizon-chart-check to render into a temp dir.
HORIZON_CHART_FILES ?= deployments/horizon-chart/files

# Sync the verification Horizon chart's bundled config from the canonical source.
# Helm's .Files is scoped to the chart directory (helm package only bundles chart files), so
# deployments/horizon-chart/files/* MUST be a copy of deployments/horizon-config/* -- it cannot
# reference the sibling dir. Re-run whenever horizon-config/* changes: the 3 apache/config files
# are copied verbatim; local_settings gets the 3 SPEC-018 env-driven edits (OPENSTACK_HOST,
# AVAILABLE_REGIONS, MEMCACHED_LOCATION) re-applied so the chart repoints Horizon at the
# in-cluster o3k + memcached Services. DEBUG needs no substitution here: local_settings reads it
# from HORIZON_DEBUG and defaults to False, so the chart is safe by default and only the dev
# docker-compose file opts into True.
horizon-chart-sync:
	@echo "Syncing $(HORIZON_CHART_FILES)/ from deployments/horizon-config/ ..."
	@mkdir -p $(HORIZON_CHART_FILES)
	cp deployments/horizon-config/config.json $(HORIZON_CHART_FILES)/config.json
	cp deployments/horizon-config/apache/ports.conf $(HORIZON_CHART_FILES)/ports.conf
	cp deployments/horizon-config/apache/horizon-nolist.conf $(HORIZON_CHART_FILES)/horizon-nolist.conf
	sed -e 's|^OPENSTACK_HOST = "o3k".*|OPENSTACK_HOST = os.environ.get("OPENSTACK_HOST", "o3k")|' -e "s|('http://o3k:35357/v3', 'RegionOne'),|(OPENSTACK_KEYSTONE_URL, 'RegionOne'),|" -e "s|'LOCATION': 'memcached:11211',|'LOCATION': os.environ.get('MEMCACHED_LOCATION', 'memcached:11211'),|" deployments/horizon-config/local_settings > $(HORIZON_CHART_FILES)/local_settings
	@echo "Synced. Review with: git diff $(HORIZON_CHART_FILES)/"

# Fail if deployments/horizon-chart/files/ is stale relative to deployments/horizon-config/.
# Non-destructive on purpose: re-renders into a temp dir and diffs, so it never rewrites the
# working tree and gives no false failures on a dirty checkout. Run by chart-test (and so by CI).
horizon-chart-check:
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
		$(MAKE) --no-print-directory horizon-chart-sync HORIZON_CHART_FILES="$$tmp" >/dev/null && \
		if diff -ru deployments/horizon-chart/files "$$tmp" >/dev/null; then \
			echo "horizon-chart files/ is in sync with deployments/horizon-config/"; \
		else \
			echo "ERROR: deployments/horizon-chart/files/ is stale:"; \
			diff -ru deployments/horizon-chart/files "$$tmp" || true; \
			echo "Run 'make horizon-chart-sync' and commit the result."; \
			exit 1; \
		fi

# Run all tests (unit + chart + contract + integration + performance)
test-all: test chart-test test-contract test-integration test-multi-tenant test-errors test-horizon-ui test-performance
	@echo ""
	@echo "All tests completed!"

# Clean build artifacts
clean:
	@echo "Cleaning..."
	rm -rf $(BUILD_DIR)

# Run comprehensive performance benchmarks (Sprint 69)
bench:
	@echo "Running comprehensive performance benchmarks..."
	@echo "Checking O3K is running..."
	@curl -s http://localhost:35357/v3 > /dev/null || (echo "ERROR: O3K not running. Run 'docker compose -f deployments/docker-compose.yml up -d' first." && exit 1)
	@bash test/benchmark/run_benchmarks.sh

# Run quick benchmarks (Go only, no k6)
bench-quick:
	@echo "Running quick Go benchmarks..."
	@echo "Cache benchmarks:"
	@go test -bench=BenchmarkCache -benchmem -benchtime=5s ./test/benchmark/ || echo "Skipped (Redis not available)"
	@echo ""
	@echo "Database benchmarks:"
	@go test -bench=BenchmarkDatabase -benchmem -benchtime=5s ./test/benchmark/ || echo "Skipped (PostgreSQL not available)"

# Run database migrations manually
migrate:
	@echo "Running migrations..."
	migrate -path migrations -database "$(DB_URL)" up

# Start PostgreSQL in Docker (for development)
db-up:
	@echo "Starting PostgreSQL..."
	docker run -d --name o3k-postgres \
		-e POSTGRES_DB=o3k \
		-e POSTGRES_USER=o3k \
		-e POSTGRES_PASSWORD=secret \
		-p 5432:5432 \
		postgres:$(POSTGRES_VERSION)
	@echo "Waiting for PostgreSQL to be ready..."
	@sleep 3

# Stop PostgreSQL
db-down:
	@echo "Stopping PostgreSQL..."
	docker stop o3k-postgres || true
	docker rm o3k-postgres || true

# Run with hot reload (requires air: go install github.com/cosmtrek/air@latest)
dev:
	@echo "Starting development mode with hot reload..."
	air

# Format code
fmt:
	@echo "Formatting code..."
	go fmt ./...

# Lint code (golangci-lint is pinned via the `tool` directive in go.mod)
lint:
	@echo "Linting code..."
	go tool golangci-lint run ./...

# Install development tools (Go tools + Helm + the helm-unittest plugin for chart tests)
install-tools:
	@echo "Installing development tools..."
	go install github.com/air-verse/air@latest
	go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
	@echo "Checking Helm CLI..."
	@if command -v helm >/dev/null 2>&1; then \
		echo "  helm present: $$(helm version --short)"; \
	else \
		echo "  helm not found - installing via the official get-helm-3 script (may prompt for sudo)..."; \
		curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash; \
	fi
	@echo "Installing/updating the helm-unittest plugin (for 'helm unittest charts/o3k')..."
	@if helm plugin list 2>/dev/null | grep -q '^unittest'; then \
		helm plugin update unittest; \
	else \
		helm plugin install https://github.com/helm-unittest/helm-unittest; \
	fi

# Install eBPF development tools (Linux only)
install-ebpf-tools:
	@echo "Installing eBPF development tools..."
	@echo "NOTE: This requires Linux. On macOS/Windows, O3K runs in stub mode."
	@if [ "$$(uname)" = "Linux" ]; then \
		echo "Installing clang, llvm, libbpf-dev..."; \
		sudo apt-get update && sudo apt-get install -y clang llvm libbpf-dev linux-headers-$$(uname -r); \
		go get github.com/cilium/ebpf@latest; \
		echo "eBPF tools installed successfully"; \
	else \
		echo "Not on Linux - eBPF tools not needed for stub mode"; \
	fi

# Show help
help:
	@echo "O3K Makefile targets:"
	@echo "  build            - Build the binary"
	@echo "  build-ebpf       - Compile eBPF programs (Linux only)"
	@echo "  build-with-ebpf  - Build binary with eBPF support"
	@echo "  run              - Build and run the application"
	@echo "  test             - Run unit tests"
	@echo "  test-contract    - Run contract tests (requires O3K running)"
	@echo "  test-integration - Run integration tests"
	@echo "  test-multi-tenant - Run multi-tenancy isolation tests"
	@echo "  test-performance - Run performance and load tests"
	@echo "  test-errors      - Run error handling tests"
	@echo "  test-horizon-ui  - Run the Horizon create-UI regression probe (requires O3K running)"
	@echo "  chart-test       - Run Helm chart tests (lint + template + unittest; no cluster)"
	@echo "  horizon-chart-sync - Sync verification Horizon chart files/ from deployments/horizon-config/"
	@echo "  horizon-chart-check - Fail if that synced copy is stale (non-destructive; run by chart-test)"
	@echo "  test-all         - Run all test suites"
	@echo "  clean            - Remove build artifacts"
	@echo "  install-deps     - Install Go dependencies"
	@echo "  migrate        - Run database migrations"
	@echo "  db-up          - Start PostgreSQL in Docker"
	@echo "  db-down        - Stop PostgreSQL container"
	@echo "  dev            - Run with hot reload (requires air)"
	@echo "  fmt            - Format code"
	@echo "  lint           - Lint code"
	@echo "  install-tools  - Install dev tools (Go tools + Helm + helm-unittest plugin)"
	@echo "  install-ebpf-tools - Install eBPF development tools (Linux only)"
