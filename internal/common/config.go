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

// LoadConfig loads configuration from file and applies environment variable overrides
func LoadConfig(path string) (*Config, error) {
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

	// Warn if using default JWT secret
	if cfg.Keystone.JWTSecret == "change-me-in-production" {
		fmt.Fprintln(os.Stderr, "WARNING: Using default JWT secret! Set LIGHTSTACK_JWT_SECRET environment variable in production.")
	}

	return &cfg, nil
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
