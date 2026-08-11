package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/cinder"
	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/compute"
	"github.com/cobaltcore-dev/lightstack/internal/console"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/glance"
	"github.com/cobaltcore-dev/lightstack/internal/health"
	"github.com/cobaltcore-dev/lightstack/internal/keystone"
	"github.com/cobaltcore-dev/lightstack/internal/metadata"
	"github.com/cobaltcore-dev/lightstack/internal/middleware"
	"github.com/cobaltcore-dev/lightstack/internal/neutron"
	"github.com/cobaltcore-dev/lightstack/internal/nova"
	"github.com/cobaltcore-dev/lightstack/internal/placement"
	"github.com/cobaltcore-dev/lightstack/internal/teardown"
	"github.com/cobaltcore-dev/lightstack/pkg/cache"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/cobaltcore-dev/lightstack/pkg/paths"
	"github.com/cobaltcore-dev/lightstack/pkg/storage"
	"github.com/gin-gonic/gin"
	zlog "github.com/rs/zerolog/log"
)

var (
	configPath     = flag.String("config", "config/lightstack.yaml", "Path to configuration file")
	migrationsPath = flag.String("migrations", "migrations", "Path to migrations directory")
)

func main() {
	// Subcommand dispatch: `lightstack cleanup` runs the SPEC-010 teardown and exits,
	// before flag.Parse (which would otherwise treat the subcommand as a positional).
	if len(os.Args) > 1 && os.Args[1] == "cleanup" {
		runCleanup(os.Args[2:])
		return
	}

	// `lightstack healthcheck` probes this node's identity-port health endpoints and
	// exits 0/1 — a local diagnosis / exec-probe alternative (SPEC-013 FR-011).
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck(os.Args[2:]))
	}

	// `lightstack console-tokens list|purge` inspects and revokes noVNC console tokens
	// (SPEC-021). The projection files under /var/lib/lightstack/console-tokens are
	// readable by hand, but they carry no expiry or ownership — both live in the
	// database, which is what this reads.
	if len(os.Args) > 1 && os.Args[1] == "console-tokens" {
		os.Exit(runConsoleTokens(os.Args[2:]))
	}

	flag.Parse()

	// Load configuration
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize structured logging
	middleware.InitLogger(&cfg.Logging)

	// Set up Gin mode based on log level
	if cfg.Logging.Level == "debug" {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	// Connect to database with optimized pool settings
	ctx := context.Background()
	poolConfig := newPoolConfig(cfg)

	pool, err := database.Connect(ctx, cfg.Database.URL, poolConfig)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer pool.Close()

	log.Printf("Database connection established (pool: max=%d, min=%d)", poolConfig.MaxConns, poolConfig.MinConns)

	// Health checker for the Kubernetes probes (SPEC-013): readiness gates on the
	// database being reachable + all listeners bound; liveness is dependency-free.
	healthChecker := health.New(zlog.Logger, health.DatabaseCheck(pool))

	// Run migrations
	log.Println("Running database migrations...")
	if err := database.MigrateUp(cfg.Database.URL, *migrationsPath); err != nil {
		log.Fatalf("Failed to run migrations: %v", err)
	}

	// Bootstrap the admin account from configuration (SPEC-007 WS-2). Fail-soft:
	// the install-blocking gate is owned by the Helm chart, not the binary.
	if err := keystone.BootstrapAdmin(ctx, pool, cfg.Keystone); err != nil {
		log.Printf("WARNING: admin bootstrap failed: %v", err)
	}

	// Initialize cache
	var cacheInstance *cache.Cache
	if cfg.Cache.Enabled {
		cacheInstance, err = cache.NewCache(cache.Config{
			RedisURL:   cfg.Cache.RedisURL,
			Enabled:    cfg.Cache.Enabled,
			KeyPrefix:  cfg.Cache.KeyPrefix,
			DefaultTTL: cfg.Cache.DefaultTTL,
		})
		if err != nil {
			log.Fatalf("Failed to initialize cache: %v", err)
		}
		log.Printf("Redis cache enabled (prefix: %s, default TTL: %v)", cfg.Cache.KeyPrefix, cfg.Cache.DefaultTTL)
	} else {
		log.Println("Cache disabled")
	}

	// Initialize services
	authService := keystone.NewAuthService(pool, cfg.Keystone.JWTSecret, cfg.Keystone.TokenTTL, cacheInstance)
	authService.SetGlanceS3Endpoint(cfg.Glance.S3Endpoint)             // advertise the RGW endpoint in the s3 catalog
	authService.SetRGWCompatTenantIDs(cfg.Keystone.RGWCompatTenantIDs) // RGW-safe project ids in token payloads
	keystoneService := keystone.NewService(pool, authService, cacheInstance)

	// Set default libvirt mode if not specified
	libvirtMode := cfg.Nova.LibvirtMode
	if libvirtMode == "" {
		libvirtMode = "stub"
	}
	novaService := nova.NewService(pool, cfg.Nova.LibvirtURI, libvirtMode, cacheInstance)

	// Initialize hypervisor
	if err := novaService.InitHypervisor(); err != nil {
		log.Printf("WARNING: Failed to initialize hypervisor: %v", err)
		log.Printf("Nova will run in %s mode", libvirtMode)
	} else {
		log.Printf("Hypervisor initialized successfully in %s mode", libvirtMode)
		if libvirtMode == "real" {
			// Before any guest can start: every domain names its console socket in this
			// directory, which libvirt does not prepare (SPEC-021 FR-017a). On failure the
			// hypervisor refuses guest starts and says why; every other service keeps running.
			if err := novaService.PrepareConsoleSocketDir(ctx, paths.ConsoleSocketDir()); err != nil {
				log.Printf("ERROR: console socket directory not ready, guest starts are refused until it is: %v", err)
			}
		}
	}

	// Set default networking mode if not specified
	networkingMode := cfg.Neutron.NetworkingMode
	if networkingMode == "" {
		networkingMode = "stub"
	}
	neutronService := neutron.NewService(pool, networkingMode, cacheInstance)
	log.Printf("Neutron initialized in %s mode", networkingMode)

	// Wire up Nova-Neutron integration (so Nova can allocate ports)
	novaService.SetNeutronService(neutronService)

	// Initialize VXLAN if enabled
	var vxlanCoordinator *neutron.VXLANCoordinator
	var nodeRegistry *compute.NodeRegistry

	if cfg.Neutron.VXLANEnabled {
		// Create node registry
		nodeRegistry, err = compute.NewNodeRegistry(
			pool,
			cfg.Compute.NodeID,
			cfg.Compute.TunnelIP,
			cfg.Compute.HeartbeatInterval,
		)
		if err != nil {
			log.Fatalf("Failed to create node registry: %v", err)
		}

		// Register node
		if err := nodeRegistry.RegisterNode(ctx); err != nil {
			log.Fatalf("Failed to register node: %v", err)
		}
		log.Printf("Node registered: %s (tunnel IP: %s)", nodeRegistry.GetHostname(), nodeRegistry.GetTunnelIP())

		// Start heartbeat
		go nodeRegistry.StartHeartbeat(ctx)

		// Create VXLAN manager
		vxlanManager := networking.NewVXLANManager(networkingMode, cfg.Compute.VXLANPort)

		// Create VXLAN coordinator
		vxlanCoordinator = neutron.NewVXLANCoordinator(
			pool,
			vxlanManager,
			nodeRegistry,
			cfg.Neutron.CoordinationPollInterval,
			cfg.Neutron.VNIRangeStart,
			cfg.Neutron.VNIRangeEnd,
		)

		// Set coordinator in neutron service
		neutronService.SetVXLANCoordinator(vxlanCoordinator)

		// Start coordinator
		go vxlanCoordinator.Start(ctx)

		log.Printf("VXLAN overlay networking enabled (VNI range: %d-%d)", cfg.Neutron.VNIRangeStart, cfg.Neutron.VNIRangeEnd)
	}

	// Real-mode network-state reconciler (SPEC-009 WS-2): on startup + periodically,
	// re-spawn DHCP (dnsmasq dies with the pod) and ensure bridges from the DB so VM
	// networking survives a control-plane restart. Real networking modes only.
	var networkReconciler *neutron.NetworkReconciler
	if networkingMode != "stub" {
		networkReconciler = neutron.NewNetworkReconciler(neutronService, cfg.Neutron.ReconcileInterval)
		go networkReconciler.Start(ctx)
		// The EFFECTIVE interval, not the configured one: NewNetworkReconciler clamps anything <= 0
		// to 30s, so logging the config made a default deployment print "interval: 0s" and read as
		// though periodic reconciliation were off (cloud-storage#546).
		log.Printf("Real-mode network reconciler started (interval: %s)", networkReconciler.PollInterval())
	}

	// Set default storage mode if not specified
	cinderStorageMode := cfg.Cinder.StorageMode
	if cinderStorageMode == "" {
		cinderStorageMode = "stub"
	}
	cinderService := cinder.NewService(pool, cinderStorageMode, cfg.Cinder.CephPool, cfg.Cinder.CephConf)
	log.Printf("Cinder initialized in %s mode", cinderStorageMode)

	// Nova creates boot volumes through Cinder (boot-from-new-volume, SPEC-020 FR-025).
	novaService.SetCinderService(cinderService)

	glanceStorageMode := cfg.Glance.StorageMode
	if glanceStorageMode == "" {
		glanceStorageMode = "stub"
	}
	glanceService, err := glance.NewService(
		pool,
		glanceStorageMode,
		cfg.Glance.CephPool,
		cfg.Glance.CephConf,
		storage.S3Options{
			Bucket:             cfg.Glance.S3Bucket,
			Region:             cfg.Glance.S3Region,
			Endpoint:           cfg.Glance.S3Endpoint,
			CABundlePath:       cfg.Glance.S3CABundle,
			InsecureSkipVerify: cfg.Glance.S3InsecureSkipVerify,
			ConnectTimeout:     cfg.Glance.S3ConnectTimeout,
		},
		cacheInstance,
	)
	if err != nil {
		log.Fatalf("Failed to initialize Glance: %v", err)
	}
	log.Printf("Glance initialized in %s mode", glanceStorageMode)

	// Share Glance's image store with Nova so real-mode boots can lazily fetch
	// an S3/RGW-backed image base on this node before staging the overlay (SPEC-014).
	novaService.SetImageStore(glanceService.ImageStore())
	// Cinder fetches the same base before a local create-from-image of an RGW-backed image.
	cinderService.SetImageStore(glanceService.ImageStore())

	// Console tokens (SPEC-021). The store is shared with Nova's console handlers;
	// the reconciler is what actually enforces expiry, because the noVNC proxy
	// resolves whatever entry it finds on disk and never checks a clock. Its
	// startup sweep also clears entries orphaned by an unclean shutdown.
	consoleStore := console.NewStore(pool, paths.ConsoleTokenDir(), cfg.Nova.ConsoleTokenTTL)
	// Real mode only: stub mode refuses console requests (501), and a dev machine
	// may not be able to write /var/lib/lightstack at all.
	if libvirtMode == "real" {
		if err := consoleStore.EnsureDir(); err != nil {
			log.Fatalf("Failed to create console token directory: %v", err)
		}
	}
	novaService.SetConsoleStore(consoleStore)
	novaService.SetConsoleProxyBaseURL(cfg.Nova.ConsoleProxyBaseURL)
	novaService.SetConsoleEnabled(cfg.Nova.ConsolesEnabled())
	// Its own cancellable context so shutdown can stop it before the pool closes.
	// On the process context it would keep sweeping through shutdown and log a
	// reaping failure against a closing pool on every restart.
	consoleCtx, stopConsoleReconciler := context.WithCancel(ctx)
	// Real mode also compares every token with the socket its guest serves now, which
	// catches the console moves that never reach the API: a guest powering itself
	// off, a crash, a restart outside Lightstack.
	var currentSocket console.CurrentSocket
	if libvirtMode == "real" {
		currentSocket = novaService.CurrentConsoleSocket
	}
	go console.NewReconciler(consoleStore, cfg.Nova.ConsoleReconcileInterval, currentSocket).Run(consoleCtx)
	log.Printf("Console tokens: dir=%s ttl=%s reconcile=%s",
		paths.ConsoleTokenDir(), cfg.Nova.ConsoleTokenTTL, cfg.Nova.ConsoleReconcileInterval)

	// Initialize metadata service. It must bind a VM-reachable address (all
	// interfaces by default) so the per-bridge 169.254.169.254 DNAT can reach it
	// in real mode (SPEC-009 WS-4).
	mdBind := cfg.Metadata.BindAddress
	if mdBind == "" {
		mdBind = ":8775"
	}
	metadataService := metadata.NewService(pool, mdBind)
	metadataService.SetTrustInstanceHeader(cfg.Metadata.TrustInstanceHeader)
	log.Printf("Metadata service initialized (bind %s)", mdBind)
	if cfg.Metadata.TrustInstanceHeader {
		log.Println("WARNING: metadata.trust_instance_header is on: any caller can read any instance's metadata and user-data. Test stacks only.")
	}

	// Initialize placement service
	placementService := placement.NewService()
	log.Println("Placement service initialized")

	// Create HTTP servers for each service
	servers := []*http.Server{
		createKeystoneServer(cfg, keystoneService, authService, healthChecker),
		createNovaServer(cfg, novaService, authService),
		createNeutronServer(cfg, neutronService, authService),
		createCinderServer(cfg, cinderService, authService),
		createGlanceServer(cfg, glanceService, authService),
		createPlacementServer(cfg, placementService, authService),
		createMetadataServer(metadataService),
	}

	// Bind all listeners up-front so readiness can honestly report "all HTTP
	// listeners are up" only once every bind has succeeded (SPEC-013 FR-001).
	// net.Listen binds synchronously and fails fast on a port clash.
	listeners := make([]net.Listener, len(servers))
	for i, srv := range servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			log.Fatalf("Failed to bind %s: %v", srv.Addr, err)
		}
		listeners[i] = ln
	}
	// Start all servers. Each goroutine signals just before it begins accepting,
	// so readiness reports "all listeners up" only once every Serve loop is
	// running — not merely once the sockets are bound (PR #70 review). Binding
	// up-front already means connections are backlogged (not refused) in the
	// interim; this closes the scheduler gap before MarkStarted.
	var serving sync.WaitGroup
	serving.Add(len(servers))
	for i, srv := range servers {
		srv, ln := srv, listeners[i] // capture loop variables
		go func() {
			log.Printf("Starting server on %s", srv.Addr)
			serving.Done()
			// Serve always returns non-nil (net/http); the only non-fatal case is
			// ErrServerClosed, which occurs solely during the coordinated shutdown
			// below (all servers, after MarkDraining → /readyz is already 503).
			// Every other Serve error is fatal, so a dead service port always takes
			// the whole process down — process-alive implies all listeners serving,
			// which is why /readyz needn't re-probe the other ports (PR #70 review).
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Server failed: %v", err)
			}
		}()
	}
	serving.Wait()
	healthChecker.MarkStarted()

	log.Println("Lightstack started successfully")
	log.Println("  - Keystone (Identity):    http://localhost:35357/v3")
	log.Println("  - Nova (Compute):         http://localhost:8774/v2.1")
	log.Println("  - Neutron (Network):      http://localhost:9696/v2.0")
	log.Println("  - Cinder (Block Storage): http://localhost:8776/v3")
	log.Println("  - Glance (Image):         http://localhost:9292/v2")
	log.Println("  - Placement:              http://localhost:8778")
	log.Println("  - Metadata Service:       http://localhost:8775")

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down servers...")

	// Flip readiness to not-ready first so the pod leaves the Service before we
	// stop serving (SPEC-013 FR-009). The chart's preStop/terminationGracePeriod
	// governs how long the platform waits for endpoint deregistration.
	healthChecker.MarkDraining()
	if d := drainDelay(); d > 0 {
		log.Printf("Draining: reporting not-ready for %s before shutdown", d)
		time.Sleep(d)
	}

	// Stop service background goroutines before closing HTTP servers
	novaService.Shutdown()
	log.Println("Nova background goroutines stopped")
	cinderService.Shutdown()
	log.Println("Cinder background goroutines stopped")
	stopConsoleReconciler()
	log.Println("Console-token reconciler stopped")

	// Stop VXLAN services
	if vxlanCoordinator != nil {
		vxlanCoordinator.Stop()
		log.Println("VXLAN coordinator stopped")
	}
	if nodeRegistry != nil {
		nodeRegistry.StopHeartbeat()
		log.Println("Node heartbeat stopped")
	}
	if networkReconciler != nil {
		networkReconciler.Stop()
		log.Println("Network reconciler stopped")
	}

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("Server shutdown error: %v", err)
		}
	}

	log.Println("Lightstack stopped")
}

// runCleanup executes the `lightstack cleanup` subcommand: the SPEC-010 release teardown
// (issue #53). Exit codes: 0 success, 1 a required class could not be purged
// (fail-closed — the blocking pre-delete hook then aborts uninstall), 2 usage or
// config error.
// loadConfig loads the config and applies state_path as the node-local root. Every
// entry point goes through here — the server, cleanup and console-tokens — so a
// subcommand can never act on a different root than the server it serves.
func loadConfig(path string) (*common.Config, error) {
	cfg, err := common.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	paths.SetBase(cfg.StatePath)
	return cfg, nil
}

// newPoolConfig builds the pgx pool settings, substituting defaults for anything
// the config file omits or sets to a non-positive value.
//
// The defaults are not cosmetic. pgxpool starts a background health-check ticker,
// and time.NewTicker panics on a non-positive interval — so a config without
// database.health_check_period (which is exactly what the Helm chart renders)
// crashed the process at startup. And pgxpool hands MaxConns to its pool as the
// size, which must be at least 1, so a config without database.max_connections
// failed to open the pool at all. Every path that opens a pool must go through
// here; a subcommand that built the struct by hand is how that panic was found.
func newPoolConfig(cfg *common.Config) *database.PoolConfig {
	pc := &database.PoolConfig{
		MaxConns:          int32(cfg.Database.MaxConnections),
		MinConns:          int32(cfg.Database.MinConnections),
		MaxConnLifetime:   cfg.Database.MaxConnLifetime,
		MaxConnIdleTime:   cfg.Database.MaxConnIdleTime,
		HealthCheckPeriod: cfg.Database.HealthCheckPeriod,
	}
	if pc.MaxConns <= 0 {
		pc.MaxConns = 20 // docs/CONFIGURATION.md's documented default
	}
	if pc.MinConns <= 0 {
		pc.MinConns = 2
	}
	if pc.MaxConnLifetime <= 0 {
		pc.MaxConnLifetime = 1 * time.Hour
	}
	if pc.MaxConnIdleTime <= 0 {
		pc.MaxConnIdleTime = 15 * time.Minute
	}
	if pc.HealthCheckPeriod <= 0 {
		pc.HealthCheckPeriod = 1 * time.Minute
	}
	return pc
}

// runConsoleTokens implements the `lightstack console-tokens` subcommand (SPEC-021,
// Article II). list and purge need a database connection — the projection directory
// alone cannot answer "when does this expire" or "whose is it" — so for them it loads
// the config and connects before delegating; usage and help need neither. Exit codes:
// 0 success, 1 operation failed, 2 usage/config error.
func runConsoleTokens(args []string) int {
	// The leading --config is consumed here so the subcommand's own flags stay
	// clean; everything after it belongs to console.RunCLI.
	fs := flag.NewFlagSet("console-tokens", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // usage is console.RunCLI's job, not the flag package's
	cfgPath := fs.String("config", "config/lightstack.yaml", "Path to configuration file")
	if err := fs.Parse(args); err != nil {
		// --help must not need a database: RunCLI prints usage without touching the
		// store, so answer before the config load and connect below.
		if errors.Is(err, flag.ErrHelp) {
			return console.RunCLI(context.Background(), nil, []string{"--help"}, os.Stdout, os.Stderr)
		}
		// Anything else is a misuse. Reporting it beats continuing: flag.Parse stops
		// at the first non-flag argument, so `list --json` never reaches here, and
		// swallowing the error would run the subcommand with the offending flag
		// silently dropped.
		fmt.Fprintf(os.Stderr, "console-tokens: %v\n", err)
		return 2
	}
	// Everything after the consumed --config belongs to console.RunCLI.
	rest := fs.Args()
	// Usage, help and an unknown subcommand need no database either, and an operator
	// asking for them may be doing so because the database is down.
	if !console.NeedsStore(rest) {
		return console.RunCLI(context.Background(), nil, rest, os.Stdout, os.Stderr)
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "console-tokens: failed to load config: %v\n", err)
		return 2
	}
	middleware.InitLogger(&cfg.Logging)

	// A short-lived CLI needs only a couple of connections, but it must still go
	// through newPoolConfig so the health-check ticker gets a positive interval.
	pc := newPoolConfig(cfg)
	pc.MaxConns = 2
	pc.MinConns = 1

	ctx := context.Background()
	pool, err := database.Connect(ctx, cfg.Database.URL, pc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "console-tokens: failed to connect to the database: %v\n", err)
		return 1
	}
	defer pool.Close()

	store := console.NewStore(pool, paths.ConsoleTokenDir(), cfg.Nova.ConsoleTokenTTL)
	return console.RunCLI(ctx, store, rest, os.Stdout, os.Stderr)
}

func runCleanup(args []string) {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	cfgPath := fs.String("config", "config/lightstack.yaml", "Path to configuration file")
	dryRun := fs.Bool("dry-run", false, "Enumerate and log what would be purged; delete nothing")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cleanup: failed to load config: %v\n", err)
		os.Exit(2)
	}
	middleware.InitLogger(&cfg.Logging)

	if err := teardown.Run(context.Background(), cfg, teardown.Options{DryRun: *dryRun}); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup failed: %v\n", err)
		os.Exit(1)
	}
}

// runHealthcheck implements the `lightstack healthcheck` subcommand (SPEC-013 FR-011):
// a local probe of this node's identity-port endpoints, usable as an exec-probe
// alternative or for host-side diagnosis. Exit codes: 0 ready/live, 1 not-ready
// or unreachable, 2 usage error.
func runHealthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	live := fs.Bool("live", false, "query /livez instead of /readyz")
	fs.Bool("ready", false, "query /readyz (the default)")
	addr := fs.String("addr", "http://localhost:35357", "base URL of the identity listener")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	path := health.ReadyzPath
	if *live {
		path = health.LivezPath
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(strings.TrimRight(*addr, "/") + path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("%s %d %s\n", path, resp.StatusCode, strings.TrimSpace(string(body)))
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}

// maxDrainDelay caps the graceful-shutdown drain pause. LIGHTSTACK_DRAIN_SECONDS is
// operator-supplied and read directly here — outside the chart's render-time
// "drainSeconds < terminationGracePeriodSeconds" guard (e.g. raw manifests or a
// bare binary) — so bound it too: a bad value must never block shutdown for an
// absurd time.
const maxDrainDelay = 5 * time.Minute

// drainDelay returns the graceful-shutdown drain pause from LIGHTSTACK_DRAIN_SECONDS
// (whole seconds; default 0 = no pause), clamped to maxDrainDelay. The chart
// sets it so the pod is removed from the Service before the process stops
// serving (SPEC-013 FR-009). Done in the app (not a preStop `sleep`) so it works
// on minimal/distroless images too.
func drainDelay() time.Duration {
	n, err := strconv.Atoi(os.Getenv("LIGHTSTACK_DRAIN_SECONDS"))
	if err != nil || n <= 0 {
		return 0
	}
	d := time.Duration(n) * time.Second
	if d > maxDrainDelay {
		log.Printf("WARNING: LIGHTSTACK_DRAIN_SECONDS=%d exceeds the %s cap; clamping", n, maxDrainDelay)
		return maxDrainDelay
	}
	return d
}

// newEngine returns the gin engine every server is built on. It trusts no proxy: gin
// otherwise believes X-Forwarded-For and X-Real-IP from any caller, so ClientIP(),
// which the request log records as remote_addr and the s3tokens denial log as
// client_ip, would be whatever address the caller chose. Upstream OpenStack parses
// proxy headers only when told to (oslo.middleware's enable_proxy_headers_parsing, off
// by default).
func newEngine() *gin.Engine {
	r := gin.New()
	if err := r.SetTrustedProxies(nil); err != nil {
		log.Fatalf("gin: %v", err)
	}
	return r
}

func createKeystoneServer(cfg *common.Config, svc *keystone.Service, authService *keystone.AuthService, checker *health.Checker) *http.Server {
	r := newEngine()
	r.Use(middleware.ErrorHandlingMiddleware())
	r.Use(middleware.LoggingMiddleware())
	r.Use(middleware.RecoveryMiddleware())
	r.Use(middleware.CORSMiddlewareWithConfig(cfg.Server.CORSAllowedOrigins))
	r.NoRoute(middleware.NotFoundHandler())
	r.HandleMethodNotAllowed = true
	r.NoMethod(middleware.MethodNotAllowedHandler())

	// Unauthenticated k8s health probes + OpenStack /healthcheck alias (SPEC-013),
	// mounted on the identity engine beside the root version route (no auth group).
	checker.Register(r)

	// Root version discovery
	r.GET("/", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"versions": gin.H{
				"values": []gin.H{
					{
						"id":     "v3.14",
						"status": "stable",
						"links": []gin.H{
							{"rel": "self", "href": fmt.Sprintf("http://localhost:%d/v3", cfg.Keystone.Port)},
						},
					},
				},
			},
		})
	})

	svc.RegisterRoutes(r.Group(""), middleware.AuthMiddleware(authService))

	return &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Keystone.Port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func createNovaServer(cfg *common.Config, svc *nova.Service, authService *keystone.AuthService) *http.Server {
	r := newEngine()
	r.Use(middleware.ErrorHandlingMiddleware())
	r.Use(middleware.LoggingMiddleware())
	r.Use(middleware.RecoveryMiddleware())
	r.Use(middleware.CORSMiddlewareWithConfig(cfg.Server.CORSAllowedOrigins))
	r.Use(middleware.AuthMiddleware(authService))
	r.NoRoute(middleware.NotFoundHandler())
	r.HandleMethodNotAllowed = true
	r.NoMethod(middleware.MethodNotAllowedHandler())

	svc.RegisterRoutes(r.Group(""))

	return &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Nova.Port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func createNeutronServer(cfg *common.Config, svc *neutron.Service, authService *keystone.AuthService) *http.Server {
	r := newEngine()
	// FIRST, and on the engine rather than the route group: Neutron answers in its own error
	// envelope, and the paths that need it most do not run inside a group — AuthMiddleware aborts
	// below, and NoRoute/NoMethod are engine-level. Ahead of ErrorHandlingMiddleware so a recovered
	// panic renders through it too (SPEC-024 FR-006/FR-008).
	r.Use(neutron.ErrorEnvelope())
	r.Use(middleware.ErrorHandlingMiddleware())
	r.Use(middleware.LoggingMiddleware())
	r.Use(middleware.RecoveryMiddleware())
	r.Use(middleware.CORSMiddlewareWithConfig(cfg.Server.CORSAllowedOrigins))
	r.Use(middleware.AuthMiddleware(authService))
	r.NoRoute(middleware.NotFoundHandler())
	r.HandleMethodNotAllowed = true
	r.NoMethod(middleware.MethodNotAllowedHandler())

	svc.RegisterRoutes(r.Group(""))

	return &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Neutron.Port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func createCinderServer(cfg *common.Config, svc *cinder.Service, authService *keystone.AuthService) *http.Server {
	r := newEngine()
	r.Use(middleware.ErrorHandlingMiddleware())
	r.Use(middleware.LoggingMiddleware())
	r.Use(middleware.RecoveryMiddleware())
	r.Use(middleware.CORSMiddlewareWithConfig(cfg.Server.CORSAllowedOrigins))
	r.Use(middleware.AuthMiddleware(authService))
	r.NoRoute(middleware.NotFoundHandler())
	r.HandleMethodNotAllowed = true
	r.NoMethod(middleware.MethodNotAllowedHandler())

	svc.RegisterRoutes(r.Group(""))

	return &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Cinder.Port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func createGlanceServer(cfg *common.Config, svc *glance.Service, authService *keystone.AuthService) *http.Server {
	r := newEngine()
	r.Use(middleware.ErrorHandlingMiddleware())
	r.Use(middleware.LoggingMiddleware())
	r.Use(middleware.RecoveryMiddleware())
	r.Use(middleware.CORSMiddlewareWithConfig(cfg.Server.CORSAllowedOrigins))
	r.NoRoute(middleware.NotFoundHandler())
	r.HandleMethodNotAllowed = true
	r.NoMethod(middleware.MethodNotAllowedHandler())

	// Version discovery endpoints (no auth required per OpenStack spec)
	root := r.Group("")
	root.GET("/", svc.GetVersions)
	root.GET("/v2", svc.GetVersionV2)

	// All other routes require authentication and are under /v2
	authGroup := r.Group("/v2")
	authGroup.Use(middleware.AuthMiddleware(authService))
	svc.RegisterRoutes(authGroup)

	return &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Glance.Port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       300 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func createPlacementServer(cfg *common.Config, svc *placement.Service, authService *keystone.AuthService) *http.Server {
	r := newEngine()
	r.Use(middleware.ErrorHandlingMiddleware())
	r.Use(middleware.LoggingMiddleware())
	r.Use(middleware.RecoveryMiddleware())
	r.Use(middleware.AuthMiddleware(authService))
	r.NoRoute(middleware.NotFoundHandler())
	r.HandleMethodNotAllowed = true
	r.NoMethod(middleware.MethodNotAllowedHandler())

	svc.RegisterRoutes(r.Group(""))

	return &http.Server{
		Addr:              ":8778",
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func createMetadataServer(svc *metadata.Service) *http.Server {
	// Guests are identified by their TCP peer address (see metadata.Service); the engine
	// trusts no proxy either, so no forwarding header speaks for the caller in its logs.
	r := newEngine()
	r.Use(middleware.ErrorHandlingMiddleware())
	r.Use(middleware.LoggingMiddleware())
	r.Use(middleware.RecoveryMiddleware())
	// No auth middleware - metadata service uses instance IP identification
	r.NoRoute(middleware.NotFoundHandler())
	r.HandleMethodNotAllowed = true
	r.NoMethod(middleware.MethodNotAllowedHandler())

	svc.RegisterRoutes(r.Group(""))

	return &http.Server{
		Addr:              svc.BindAddr(),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}
