package common

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Database DatabaseConfig `yaml:"database"`
	Compute  ComputeConfig  `yaml:"compute"`
	Keystone KeystoneConfig `yaml:"keystone"`
	Nova     NovaConfig     `yaml:"nova"`
	Neutron  NeutronConfig  `yaml:"neutron"`
	Cinder   CinderConfig   `yaml:"cinder"`
	Glance   GlanceConfig   `yaml:"glance"`
	Metadata MetadataConfig `yaml:"metadata"`
	Logging  LoggingConfig  `yaml:"logging"`
	Cache    CacheConfig    `yaml:"cache"`
	Server   ServerConfig   `yaml:"server"`
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
}

type NovaConfig struct {
	Port          int    `yaml:"port"`
	LibvirtURI    string `yaml:"libvirt_uri"`
	DefaultFlavor string `yaml:"default_flavor"`
	LibvirtMode   string `yaml:"libvirt_mode"` // "stub" or "real"
}

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

	// Warn if using default JWT secret
	if cfg.Keystone.JWTSecret == "change-me-in-production" {
		fmt.Fprintln(os.Stderr, "WARNING: Using default JWT secret! Set LIGHTSTACK_JWT_SECRET environment variable in production.")
	}

	return &cfg, nil
}
