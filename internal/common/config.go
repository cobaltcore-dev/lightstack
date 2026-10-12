package common

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
	"gopkg.in/yaml.v3"
)

type Config struct {
	// StatePath is the node-local root for per-node state (pkg/paths): image bases,
	// volumes, instance overlays, cloud-init ISOs, DHCP state and console tokens.
	// Empty means /var/lib/lightstack; the chart sets it from realMode.hostStoragePath.
	StatePath string         `yaml:"state_path"`
	Database  DatabaseConfig `yaml:"database"`
	Compute   ComputeConfig  `yaml:"compute"`
	Keystone  KeystoneConfig `yaml:"keystone"`
	Nova      NovaConfig     `yaml:"nova"`
	Neutron   NeutronConfig  `yaml:"neutron"`
	Cinder    CinderConfig   `yaml:"cinder"`
	Glance    GlanceConfig   `yaml:"glance"`
	Metadata  MetadataConfig `yaml:"metadata"`
	Logging   LoggingConfig  `yaml:"logging"`
	Cache     CacheConfig    `yaml:"cache"`
	Server    ServerConfig   `yaml:"server"`
}

type ServerConfig struct {
	CORSAllowedOrigins []string `yaml:"cors_allowed_origins"`
}

type DatabaseConfig struct {
	URL               string        `yaml:"url"`
	MaxConnections    int           `yaml:"max_connections"`
	MinConnections    int           `yaml:"min_connections"`
	MaxConnLifetime   time.Duration `yaml:"max_conn_lifetime"`
	MaxConnIdleTime   time.Duration `yaml:"max_conn_idle_time"`
	HealthCheckPeriod time.Duration `yaml:"health_check_period"`
}

type ComputeConfig struct {
	NodeID            string        `yaml:"node_id"`
	TunnelIP          string        `yaml:"tunnel_ip"`
	VXLANPort         int           `yaml:"vxlan_port"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
}

type KeystoneConfig struct {
	Port          int           `yaml:"port"`
	JWTSecret     string        `yaml:"jwt_secret"`
	TokenTTL      time.Duration `yaml:"token_ttl"`
	AdminUser     string        `yaml:"admin_user"`
	AdminPassword string        `yaml:"admin_password"`
	// RGWCompatTenantIDs renders token.project.id in OpenStack's established
	// dash-free form — Ceph RGW derives the Swift tenant name and implicit user
	// id from that field and only accepts [A-Za-z0-9_] in tenant names. It affects
	// /v3/auth/tokens; /v3/s3tokens already uses the dash-free form. The EC2 mint
	// accepts that form as input and stores the canonical database UUID. Everything
	// else keeps canonical (hyphenated) UUIDs.
	RGWCompatTenantIDs bool `yaml:"rgw_compat_tenant_ids"`
}

type NovaConfig struct {
	Port          int    `yaml:"port"`
	LibvirtURI    string `yaml:"libvirt_uri"`
	DefaultFlavor string `yaml:"default_flavor"`
	LibvirtMode   string `yaml:"libvirt_mode"` // "stub" or "real"

	// ConsoleProxyBaseURL is the browser-reachable ORIGIN of the console proxy
	// ("scheme://host:port"), never a full page URL: the client page and the
	// ?path=%3Ftoken%3D query are appended in code so they cannot drift from the
	// deployed noVNC/websockify version (SPEC-021 FR-020). Empty means "derive
	// from the same endpoint settings as the rest of the catalog" (FR-021).
	// Anything but an origin fails LoadConfig (validateConsoleProxyBaseURL).
	ConsoleProxyBaseURL string `yaml:"console_proxy_base_url"`
	// ConsoleTokenTTL bounds how long an issued console link stays usable
	// (FR-010). Note the effective ceiling is TTL + ConsoleReconcileInterval:
	// the proxy performs no expiry check of its own, so a token dies when Lightstack
	// removes its projection entry, not at the instant it expires.
	ConsoleTokenTTL time.Duration `yaml:"console_token_ttl"`
	// ConsoleReconcileInterval is how often expired console tokens are reaped
	// from the database and the projection directory (FR-016). This is what
	// actually enforces expiry — see ConsoleTokenTTL.
	ConsoleReconcileInterval time.Duration `yaml:"console_reconcile_interval"`
	// ConsoleEnabled says whether a console proxy is deployed at all. Unset means
	// yes — the deploy scripts run one and never set it. The chart renders false
	// wherever it deploys none (multi-node, where v1 has none, or
	// consoleProxy.enabled=false), so the API refuses instead of issuing a URL that
	// leads nowhere. A pointer, because a plain bool cannot tell unset from false.
	ConsoleEnabled *bool `yaml:"console_enabled"`

	// StateReconcileInterval is how often the recorded run state of real-mode
	// instances is compared against the hypervisor and corrected (SPEC-025
	// FR-024b, FR-024d).
	//
	// Only drift the product did NOT cause needs this: every action that changes a
	// guest's run state persists its own result (FR-024a), so a crashed guest or one
	// removed out of band is what the sweep is for. Reading a server never contacts
	// the hypervisor (FR-024c), which is why truth is bounded by this interval
	// rather than instantaneous.
	StateReconcileInterval time.Duration `yaml:"state_reconcile_interval"`

	// ShutdownTimeout is how long a guest OS is given to shut down cleanly before it
	// is powered off the hard way (SPEC-025 FR-022).
	//
	// libvirt's shutdown is a REQUEST to the guest, not a transition: it can take
	// tens of seconds and a guest that ignores ACPI never honours it at all. Without
	// a bound, a stop either lies about the outcome or waits forever. Mirrors
	// upstream's shutdown_timeout, including its 60s default.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`

	// InstanceActionRetention is how long a server's action records are kept after
	// the server itself is deleted (SPEC-025 FR-042a, FR-042b).
	//
	// Deleting a server does NOT delete its action history — upstream retains it,
	// and so do we — so without a retention age the table only ever grows. Records
	// belonging to a server that still exists are never reclaimed, whatever their
	// age (FR-042c).
	InstanceActionRetention time.Duration `yaml:"instance_action_retention"`
}

// ConsolesEnabled reports whether a console proxy is deployed; unset means yes.
func (n NovaConfig) ConsolesEnabled() bool {
	return n.ConsoleEnabled == nil || *n.ConsoleEnabled
}

// Console defaults (SPEC-021). Applied by LoadConfig when the file omits them,
// so a deployment that never heard of the console still expires tokens.
const (
	DefaultConsoleTokenTTL          = 10 * time.Minute
	DefaultConsoleReconcileInterval = 30 * time.Second
)

// Compute state and audit-retention defaults (SPEC-025). Applied by LoadConfig when
// the file omits them: a deployment that never heard of either setting must still
// reconcile drift and still bound its audit growth.
const (
	// DefaultStateReconcileInterval matches the 30s resync period of the real-mode
	// network reconciler (NeutronConfig.ReconcileInterval) — the same order of
	// magnitude suits both, and one number is easier to reason about than two.
	DefaultStateReconcileInterval = 30 * time.Second
	// DefaultInstanceActionRetention is 90 days: an ordinary audit-retention period.
	// Any bounded value satisfies FR-042a; an operator who cares changes it.
	DefaultInstanceActionRetention = 90 * 24 * time.Hour
	// DefaultShutdownTimeout is deliberately BELOW upstream's 60s. Upstream's stop is
	// asynchronous — nothing holds an HTTP connection while the guest shuts down — so
	// it can afford a minute. Lightstack's is synchronous, which makes the grace period a
	// request duration, and 30s is the default read timeout of gunicorn (Horizon) and
	// of most ingresses. A default above that fails in a deployment, not in a test.
	DefaultShutdownTimeout = 30 * time.Second
)

// Bounds on how long a synchronous request may take to answer (SPEC-025 FR-022b).
//
// These three are one arithmetic statement, which is why they live together: a handler
// that runs past the server's write deadline has its connection closed by net/http with
// NO response written. The stop still happens; it simply cannot report that it did,
// which is the failure this feature exists to remove, reintroduced a layer down. Found
// on a host with the grace period and the write deadline both at 60s, where every
// forced power-off lost its answer and the client retried into a conflict.
const (
	// HTTPWriteTimeout is the write deadline every service's HTTP server uses. net/http
	// resets it when a request's header is read, so it covers handler execution PLUS
	// the response write — not just the write, as the name suggests.
	HTTPWriteTimeout = 60 * time.Second

	// LifecycleResponseSlack is what a lifecycle request needs on top of the guest's
	// grace period: the state lookups, the forced power-off, the record write and the
	// response itself.
	LifecycleResponseSlack = 10 * time.Second

	// MaxShutdownTimeout is the longest grace a synchronous stop can be given and still
	// answer. A larger configured value is clamped to it.
	MaxShutdownTimeout = HTTPWriteTimeout - LifecycleResponseSlack
)

type NeutronConfig struct {
	Port                     int           `yaml:"port"`
	DHCPLeaseTime            time.Duration `yaml:"dhcp_lease_time"`
	IPTablesEnabled          bool          `yaml:"iptables_enabled"`
	NetworkingMode           string        `yaml:"networking_mode"` // "stub", "iptables", or "ebpf"
	VXLANEnabled             bool          `yaml:"vxlan_enabled"`
	VNIRangeStart            int           `yaml:"vni_range_start"`
	VNIRangeEnd              int           `yaml:"vni_range_end"`
	CoordinationPollInterval time.Duration `yaml:"coordination_poll_interval"`
	VXLANMTU                 int           `yaml:"vxlan_mtu"`
	// ReconcileInterval is the resync period for the real-mode network-state
	// reconciler (WS-2). Zero means consumers apply a default (30s). Only used
	// when NetworkingMode != "stub".
	ReconcileInterval time.Duration `yaml:"reconcile_interval"`
}

type CinderConfig struct {
	Port        int    `yaml:"port"`
	CephPool    string `yaml:"ceph_pool"`
	CephConf    string `yaml:"ceph_conf"`
	StorageMode string `yaml:"storage_mode"` // "stub" or "real"
}

type GlanceConfig struct {
	Port        int    `yaml:"port"`
	CephPool    string `yaml:"ceph_pool"`
	CephConf    string `yaml:"ceph_conf"`
	StorageMode string `yaml:"storage_mode"` // "stub", "local", "rbd", "s3", or combinations
	S3Bucket    string `yaml:"s3_bucket"`    // S3 bucket name
	S3Region    string `yaml:"s3_region"`    // S3 region
	S3Endpoint  string `yaml:"s3_endpoint"`  // Custom S3 endpoint (for MinIO, Ceph RGW, etc.)
	// S3CABundle is a path to a PEM CA bundle to trust for an HTTPS S3/RGW
	// endpoint backed by an internal/private CA. Empty uses the system trust
	// store (fine for HTTP or publicly-trusted certs).
	S3CABundle string `yaml:"s3_ca_bundle"`
	// S3InsecureSkipVerify disables TLS verification for the S3/RGW endpoint.
	// TEST ONLY — never enable in production; defaults to false.
	S3InsecureSkipVerify bool `yaml:"s3_insecure_skip_verify"`
	// S3ConnectTimeout bounds connect + time-to-first-byte (fail-fast). It does
	// NOT bound the body transfer, so large images stream to completion. Zero
	// means the storage layer applies a default.
	S3ConnectTimeout time.Duration `yaml:"s3_connect_timeout"`
}

// MetadataConfig configures the EC2/OpenStack metadata service (WS-4).
type MetadataConfig struct {
	// BindAddress is the host address the metadata server listens on. In real
	// mode it MUST be reachable from tenant network namespaces (not localhost);
	// the per-namespace 169.254.169.254 DNAT redirects to it. Zero means
	// consumers apply a default ("0.0.0.0:8775").
	BindAddress string `yaml:"bind_address"`

	// TrustInstanceHeader lets any caller name the instance it asks about with the
	// X-Instance-ID header, instead of being identified by its own address; loopback
	// callers may do so regardless. It exists for test stacks whose requests arrive from
	// elsewhere (Docker port mapping presents them from the bridge gateway) and must stay
	// off wherever real guests reach the metadata service: with it on, any guest can read
	// any instance's metadata and user-data.
	TrustInstanceHeader bool `yaml:"trust_instance_header"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type CacheConfig struct {
	Enabled    bool                     `yaml:"enabled"`
	RedisURL   string                   `yaml:"redis_url"`
	KeyPrefix  string                   `yaml:"key_prefix"`
	DefaultTTL time.Duration            `yaml:"default_ttl"`
	TTL        map[string]time.Duration `yaml:"ttl"` // Per-resource TTL overrides
}

// LoadConfig loads configuration from file, applies environment variable
// overrides, and enforces the JWT-secret guard (S2): the caller runs in the
// server's trust domain — it signs or validates tokens — so a well-known or
// empty secret must be refused rather than warned about.
func LoadConfig(path string) (*Config, error) {
	return loadConfig(path, true)
}

// LoadConfigForMigration loads configuration for the migration tool, which
// never signs or validates a token — it reads database.url and logging only.
// The secret guard protects against token forgery, not database access, so it
// must not gate `lightstack-migrate`: shipped deployment paths (the image
// entrypoint and the documented Kubernetes init container) run
// `lightstack-migrate up` against the canned config before any secret is
// wired into the environment.
func LoadConfigForMigration(path string) (*Config, error) {
	return loadConfig(path, false)
}

// loadConfig is the shared loader; enforceSecretGuard selects the S2 check.
func loadConfig(path string, enforceSecretGuard bool) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Environment variable overrides
	if dbURL := os.Getenv("LIGHTSTACK_DB_URL"); dbURL != "" {
		cfg.Database.URL = dbURL
	}
	if jwtSecret := os.Getenv("LIGHTSTACK_JWT_SECRET"); jwtSecret != "" {
		cfg.Keystone.JWTSecret = jwtSecret
	}
	// Ceph RGW compatibility (keystone.rgw_compat_tenant_ids). Two-way override:
	// an operator must be able to flip it from the env in either direction when
	// the config file is mounted read-only, matching the Glance S3 boolean
	// pattern below. Unrecognized/empty values keep the file value.
	switch os.Getenv("LIGHTSTACK_KEYSTONE_RGW_COMPAT_TENANT_IDS") {
	case "true", "1":
		cfg.Keystone.RGWCompatTenantIDs = true
	case "false", "0":
		cfg.Keystone.RGWCompatTenantIDs = false
	}
	// Glance S3/RGW overrides (handy when config is mounted read-only).
	if v := os.Getenv("LIGHTSTACK_GLANCE_STORAGE_MODE"); v != "" {
		cfg.Glance.StorageMode = v
	}
	if v := os.Getenv("LIGHTSTACK_GLANCE_S3_ENDPOINT"); v != "" {
		cfg.Glance.S3Endpoint = v
	}
	if v := os.Getenv("LIGHTSTACK_GLANCE_S3_BUCKET"); v != "" {
		cfg.Glance.S3Bucket = v
	}
	if v := os.Getenv("LIGHTSTACK_GLANCE_S3_REGION"); v != "" {
		cfg.Glance.S3Region = v
	}
	if v := os.Getenv("LIGHTSTACK_GLANCE_S3_CA_BUNDLE"); v != "" {
		cfg.Glance.S3CABundle = v
	}
	// Two-way override: an operator must be able to turn skip-verify OFF from the
	// env even if the mounted config shipped it on (matches OpenStack's symmetric
	// `insecure` config booleans). Unrecognized/empty values keep the file value.
	// Test stacks only; see MetadataConfig.TrustInstanceHeader.
	switch os.Getenv("LIGHTSTACK_METADATA_TRUST_INSTANCE_HEADER") {
	case "true", "1":
		cfg.Metadata.TrustInstanceHeader = true
	case "false", "0":
		cfg.Metadata.TrustInstanceHeader = false
	}
	switch os.Getenv("LIGHTSTACK_GLANCE_S3_INSECURE_SKIP_VERIFY") {
	case "true", "1":
		cfg.Glance.S3InsecureSkipVerify = true
	case "false", "0":
		cfg.Glance.S3InsecureSkipVerify = false
	}
	// A malformed duration is ignored (the file value stands) and warned about,
	// rather than silently zeroing the timeout.
	if v := os.Getenv("LIGHTSTACK_GLANCE_S3_CONNECT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Glance.S3ConnectTimeout = d
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: ignoring invalid LIGHTSTACK_GLANCE_S3_CONNECT_TIMEOUT %q: %v\n", v, err)
		}
	}

	// Console defaults (SPEC-021). A zero TTL would mean "already expired" and a
	// zero interval would disable the reaper entirely — since the proxy never
	// checks expiry itself, that would leave tokens valid forever. Default rather
	// than trust the file to mention them.
	if cfg.Nova.ConsoleTokenTTL <= 0 {
		cfg.Nova.ConsoleTokenTTL = DefaultConsoleTokenTTL
	}
	if cfg.Nova.ConsoleReconcileInterval <= 0 {
		cfg.Nova.ConsoleReconcileInterval = DefaultConsoleReconcileInterval
	}
	if err := validateConsoleProxyBaseURL(cfg.Nova.ConsoleProxyBaseURL); err != nil {
		return nil, err
	}
	statePath, err := resolveStatePath(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	cfg.StatePath = statePath
	if cfg.Nova.LibvirtMode == "real" && len(statePath) > maxRealModeStatePath {
		return nil, fmt.Errorf("state_path %q is %d characters; real mode allows at most %d, because "+
			"every guest's console socket (<state_path>/console-sockets/<instance-uuid>.sock) must fit "+
			"QEMU's 108-byte socket-path limit", statePath, len(statePath), maxRealModeStatePath)
	}

	// Compute state and audit-retention defaults (SPEC-025 FR-024d, FR-042b). A
	// non-positive value is clamped rather than honored: zero would mean a busy loop
	// for the interval, and "reclaim everything immediately" for the retention age —
	// the opposite of what an operator writing 0 could plausibly want.
	if cfg.Nova.StateReconcileInterval <= 0 {
		cfg.Nova.StateReconcileInterval = DefaultStateReconcileInterval
	}
	if cfg.Nova.InstanceActionRetention <= 0 {
		cfg.Nova.InstanceActionRetention = DefaultInstanceActionRetention
	}
	// Clamped like its siblings rather than honouring 0 as upstream's "force
	// immediately": unset yaml is indistinguishable from a deliberate 0, and the
	// safe reading of "not configured" is not "hard-kill every guest".
	if cfg.Nova.ShutdownTimeout <= 0 {
		cfg.Nova.ShutdownTimeout = DefaultShutdownTimeout
	}
	// Clamped rather than refused at the top end: asking for a longer grace than a
	// synchronous stop can deliver is a reasonable request of an implementation that
	// cannot honour it, and refusing to start over it would be worse than shortening
	// it and saying so.
	if cfg.Nova.ShutdownTimeout > MaxShutdownTimeout {
		fmt.Fprintf(os.Stderr,
			"WARNING: nova.shutdown_timeout %v exceeds the %v a synchronous stop can answer within; using %v\n",
			cfg.Nova.ShutdownTimeout, MaxShutdownTimeout, MaxShutdownTimeout)
		cfg.Nova.ShutdownTimeout = MaxShutdownTimeout
	}

	// Refuse well-known (or empty) JWT secrets unless explicitly overridden.
	// The defaults below ship in this repository and in .env.example, so anyone
	// can read them — and because tokens are stateless, knowing the secret is
	// enough to mint an admin token for every service (review finding S2,
	// docs/review/comprehensive-review-report-2026-10-12.md). The chart already
	// fails fast on an empty secret; this gives the binary the same stance.
	// Skipped by LoadConfigForMigration (the migration tool is outside the
	// token trust domain).
	if enforceSecretGuard && isInsecureDefaultJWTSecret(cfg.Keystone.JWTSecret) && !allowInsecureDefaultJWTSecret() {
		return nil, fmt.Errorf(
			"keystone.jwt_secret is empty or a well-known default — refusing to start. " +
				"Set keystone.jwt_secret in the config file (or the LIGHTSTACK_JWT_SECRET " +
				"environment variable) to a random value, e.g. `openssl rand -base64 32`. " +
				"For isolated development or test environments only, " +
				"LIGHTSTACK_ALLOW_INSECURE_DEFAULT_SECRET=1 opts out of this check")
	}

	return &cfg, nil
}

// insecureDefaultJWTSecrets are values that appear in this repository's shipped
// configuration and examples. An empty secret is equally forgeable (HMAC with a
// known key), so it is refused too.
var insecureDefaultJWTSecrets = map[string]struct{}{
	"":                        {},
	"change-me-in-production": {},
	"change-me-in-production-use-openssl-rand-base64-32": {},
}

func isInsecureDefaultJWTSecret(secret string) bool {
	_, known := insecureDefaultJWTSecrets[secret]
	return known
}

// allowInsecureDefaultJWTSecret is the documented escape hatch for stub-mode
// development and test stacks that never validate a foreign token.
func allowInsecureDefaultJWTSecret() bool {
	switch os.Getenv("LIGHTSTACK_ALLOW_INSECURE_DEFAULT_SECRET") {
	case "1", "true":
		return true
	default:
		return false
	}
}

// maxRealModeStatePath keeps a guest's console socket path within QEMU's 108-byte
// sun_path: 108 - len("/console-sockets/") - 36 (uuid) - len(".sock") = 50 (SPEC-021
// FR-017). Stub mode creates no sockets, so it is not limited.
const maxRealModeStatePath = 50

// resolveStatePath defaults and validates the node-local root (pkg/paths). It must be
// absolute: libvirtd on the host opens the paths Lightstack writes into the domain XML,
// so Lightstack and the host must read the same string. "/" is refused because cleanup
// purges the per-service subdirectories directly under the root.
func resolveStatePath(p string) (string, error) {
	if p == "" {
		return paths.DefaultBase, nil
	}
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) || clean == "/" {
		return "", fmt.Errorf("state_path %q must be an absolute directory other than /", p)
	}
	return clean, nil
}

// validateConsoleProxyBaseURL refuses anything but an origin (SPEC-021 FR-020).
// Lightstack appends the client page and the token query itself, so a path would
// yield a console link that 404s — and nothing would say why until a browser
// tried it. The trap is upstream's: Nova's novncproxy_base_url DOES name the page
// (".../vnc_auto.html"), so values carried over from Nova, or from our own old
// guides, arrive with one. A trailing slash is harmless; buildConsoleURL trims it.
func validateConsoleProxyBaseURL(raw string) error {
	if raw == "" {
		return nil // derived from the request (FR-021)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("nova.console_proxy_base_url %q is not an origin: want http(s)://host[:port]", raw)
	}
	// Comparing against the rebuilt origin catches every extra at once: path,
	// query, fragment, userinfo, even a bare trailing "?" or "#".
	origin := u.Scheme + "://" + u.Host
	if strings.TrimSuffix(raw, "/") != origin {
		return fmt.Errorf("nova.console_proxy_base_url %q must be an origin only; Lightstack appends "+
			"the client page and token itself (unlike upstream Nova's novncproxy_base_url, it names "+
			"no page): use %q", u.Redacted(), origin)
	}
	return nil
}
