.PHONY: build run test chart-test test-endpoint-migration test-compose-identity clean install-deps install-tools migrate db-up db-down build-ebpf bench bench-quick

# Build variables
BINARY_NAME=lightstack
BUILD_DIR=bin
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-X main.Version=$(VERSION)"

# Database variables
POSTGRES_VERSION?=18
# Development-only defaults. Do NOT use in production.
# Production should set DB_URL via environment variable with sslmode=require.
DB_URL?=postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable

# Build the binary
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/lightstack

# Build eBPF programs
build-ebpf:
	@echo "Building eBPF programs..."
	@which clang > /dev/null || (echo "ERROR: clang not found. Install with: apt-get install clang llvm libbpf-dev" && exit 1)
	clang -O2 -target bpf -c pkg/networking/ebpf/secgroup.c -o pkg/networking/ebpf/secgroup.o
	@echo "eBPF program compiled: pkg/networking/ebpf/secgroup.o"

# Build with eBPF support
build-with-ebpf: build-ebpf build
	@echo "Built Lightstack with eBPF support"

# Run the application
run: build
	@echo "Starting lightstack..."
	./$(BUILD_DIR)/$(BINARY_NAME) --config config/lightstack.yaml --migrations migrations

# Install dependencies
install-deps:
	@echo "Installing dependencies..."
	go mod download
	go mod tidy

# Run tests
test:
	@echo "Running tests..."
	go test -v ./...

# Run contract tests (requires Lightstack to be running)
test-contract:
	@echo "Running contract tests..."
	@echo "Checking Lightstack is running..."
	@curl -s http://localhost:35357/v3 > /dev/null || (echo "ERROR: Lightstack not running. Run 'docker compose -f deployments/docker-compose.yml up -d' or 'make run' first." && exit 1)
	@echo "Running gophercloud contract tests..."
	go test -v ./test/contract/... -timeout 5m

# Run integration tests (requires Lightstack to be running)
test-integration:
	@echo "Running integration tests..."
	@echo "Checking Lightstack is running..."
	@curl -s http://localhost:35357/v3 > /dev/null || (echo "ERROR: Lightstack not running. Run 'docker compose -f deployments/docker-compose.yml up -d' first." && exit 1)
	@echo "Running integration test suite..."
	@bash test/integration_test.sh

# Run multi-tenancy tests
test-multi-tenant:
	@echo "Running multi-tenancy tests..."
	@echo "Checking Lightstack is running..."
	@curl -s http://localhost:35357/v3 > /dev/null || (echo "ERROR: Lightstack not running." && exit 1)
	@bash test/multi_tenant_test.sh

# Run performance tests
test-performance:
	@echo "Running performance tests..."
	@echo "Checking Lightstack is running..."
	@curl -s http://localhost:35357/v3 > /dev/null || (echo "ERROR: Lightstack not running." && exit 1)
	@bash test/performance_test.sh

# Run error handling tests
test-errors:
	@echo "Running error handling tests..."
	@echo "Checking Lightstack is running..."
	@curl -s http://localhost:35357/v3 > /dev/null || (echo "ERROR: Lightstack not running." && exit 1)
	@bash test/error_handling_test.sh

# Verify the endpoint-hostname forward migration (cloud-storage#575). Needs a PostgreSQL it
# may create a scratch database in; the database named in the URL is never touched. No
# running Lightstack required.
test-endpoint-migration:
	@echo "Running endpoint-hostname migration tests..."
	@test -n "$(LIGHTSTACK_TEST_DB_URL)" || (echo "ERROR: set LIGHTSTACK_TEST_DB_URL, e.g. postgres://lightstack:secret@127.0.0.1:5432/postgres" && exit 1)
	@bash test/endpoint_hostname_migration_test.sh

# Check that the Compose stacks' database identity is overridable (cloud-storage#575).
# Renders each stack with `compose config`; no containers started, no running Lightstack.
test-compose-identity:
	@echo "Running Compose database-identity tests..."
	@docker compose version >/dev/null 2>&1 || (echo "ERROR: 'docker compose' not found." && exit 1)
	@bash test/compose_db_identity_test.sh

# Run Helm chart tests (helm lint + template + unittest; no cluster or running Lightstack needed)
chart-test:
	@echo "Running Helm chart tests (lint + template + unittest)..."
	@command -v helm >/dev/null 2>&1 || (echo "ERROR: helm not found. Run 'make install-tools' first." && exit 1)
	@helm plugin list 2>/dev/null | grep -q '^unittest' || (echo "ERROR: helm-unittest plugin not found. Run 'make install-tools' first." && exit 1)
	@bash charts/lightstack/ci/run-tests.sh

# Run all tests (unit + chart + contract + integration + performance)
test-all: test chart-test test-contract test-integration test-multi-tenant test-errors test-performance
	@echo ""
	@echo "All tests completed!"

# Clean build artifacts
clean:
	@echo "Cleaning..."
	rm -rf $(BUILD_DIR)

# Run comprehensive performance benchmarks (Sprint 69)
bench:
	@echo "Running comprehensive performance benchmarks..."
	@echo "Checking Lightstack is running..."
	@curl -s http://localhost:35357/v3 > /dev/null || (echo "ERROR: Lightstack not running. Run 'docker compose -f deployments/docker-compose.yml up -d' first." && exit 1)
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
	docker run -d --name lightstack-postgres \
		-e POSTGRES_DB=lightstack \
		-e POSTGRES_USER=lightstack \
		-e POSTGRES_PASSWORD=secret \
		-p 5432:5432 \
		postgres:$(POSTGRES_VERSION)
	@echo "Waiting for PostgreSQL to be ready..."
	@sleep 3

# Stop PostgreSQL
db-down:
	@echo "Stopping PostgreSQL..."
	docker stop lightstack-postgres || true
	docker rm lightstack-postgres || true

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
	@echo "Installing/updating the helm-unittest plugin (for 'helm unittest charts/lightstack')..."
	@if helm plugin list 2>/dev/null | grep -q '^unittest'; then \
		helm plugin update unittest; \
	else \
		helm plugin install https://github.com/helm-unittest/helm-unittest; \
	fi

# Install eBPF development tools (Linux only)
install-ebpf-tools:
	@echo "Installing eBPF development tools..."
	@echo "NOTE: This requires Linux. On macOS/Windows, Lightstack runs in stub mode."
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
	@echo "Lightstack Makefile targets:"
	@echo "  build            - Build the binary"
	@echo "  build-ebpf       - Compile eBPF programs (Linux only)"
	@echo "  build-with-ebpf  - Build binary with eBPF support"
	@echo "  run              - Build and run the application"
	@echo "  test             - Run unit tests"
	@echo "  test-contract    - Run contract tests (requires Lightstack running)"
	@echo "  test-integration - Run integration tests"
	@echo "  test-multi-tenant - Run multi-tenancy isolation tests"
	@echo "  test-performance - Run performance and load tests"
	@echo "  test-errors      - Run error handling tests"
	@echo "  chart-test       - Run Helm chart tests (lint + template + unittest; no cluster)"
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
