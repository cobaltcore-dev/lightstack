package common

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// allowInsecureDefaultSecret opts this test into the development escape hatch:
// these tests exercise unrelated config keys through minimal config files that
// do not set keystone.jwt_secret, which LoadConfig refuses by default (S2).
func allowInsecureDefaultSecret(t *testing.T) {
	t.Helper()
	t.Setenv("LIGHTSTACK_ALLOW_INSECURE_DEFAULT_SECRET", "1")
}

func TestLoadConfig(t *testing.T) {
	// Create a temporary config file
	tmpfile, err := os.CreateTemp("", "lightstack-test-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	configContent := `
database:
  url: "postgres://test:test@localhost/test"
  max_connections: 10

keystone:
  port: 5000
  jwt_secret: "test-secret"
  token_ttl: 24h
  admin_user: "admin"
  admin_password: "test-admin-password"

nova:
  port: 8774
  libvirt_uri: "qemu:///system"
  default_flavor: "m1.small"

neutron:
  port: 9696
  dhcp_lease_time: 24h
  iptables_enabled: true

cinder:
  port: 8776
  ceph_pool: "volumes"
  ceph_conf: "/etc/ceph/ceph.conf"

glance:
  port: 9292
  ceph_pool: "images"
  ceph_conf: "/etc/ceph/ceph.conf"

logging:
  level: "info"
  format: "json"
`

	if _, err := tmpfile.Write([]byte(configContent)); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	tmpfile.Close()

	config, err := LoadConfig(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify database config
	if config.Database.URL != "postgres://test:test@localhost/test" {
		t.Errorf("Expected database URL postgres://test:test@localhost/test, got %s", config.Database.URL)
	}

	if config.Database.MaxConnections != 10 {
		t.Errorf("Expected max connections 10, got %d", config.Database.MaxConnections)
	}

	// Verify keystone config
	if config.Keystone.Port != 5000 {
		t.Errorf("Expected keystone port 5000, got %d", config.Keystone.Port)
	}

	if config.Keystone.JWTSecret != "test-secret" {
		t.Errorf("Expected JWT secret test-secret, got %s", config.Keystone.JWTSecret)
	}

	if config.Keystone.AdminUser != "admin" {
		t.Errorf("Expected admin user admin, got %s", config.Keystone.AdminUser)
	}

	// Verify nova config
	if config.Nova.Port != 8774 {
		t.Errorf("Expected nova port 8774, got %d", config.Nova.Port)
	}

	if config.Nova.LibvirtURI != "qemu:///system" {
		t.Errorf("Expected libvirt URI qemu:///system, got %s", config.Nova.LibvirtURI)
	}

	// Verify neutron config
	if config.Neutron.Port != 9696 {
		t.Errorf("Expected neutron port 9696, got %d", config.Neutron.Port)
	}

	if !config.Neutron.IPTablesEnabled {
		t.Error("Expected iptables enabled")
	}

	// Verify cinder config
	if config.Cinder.Port != 8776 {
		t.Errorf("Expected cinder port 8776, got %d", config.Cinder.Port)
	}

	if config.Cinder.CephPool != "volumes" {
		t.Errorf("Expected ceph pool volumes, got %s", config.Cinder.CephPool)
	}

	// Verify glance config
	if config.Glance.Port != 9292 {
		t.Errorf("Expected glance port 9292, got %d", config.Glance.Port)
	}

	if config.Glance.CephPool != "images" {
		t.Errorf("Expected ceph pool images, got %s", config.Glance.CephPool)
	}

	// Verify logging config
	if config.Logging.Level != "info" {
		t.Errorf("Expected log level info, got %s", config.Logging.Level)
	}

	if config.Logging.Format != "json" {
		t.Errorf("Expected log format json, got %s", config.Logging.Format)
	}
}

// keystone.rgw_compat_tenant_ids parses from YAML and defaults to false.
func TestLoadConfigKeystoneRGWCompatTenantIDs(t *testing.T) {
	allowInsecureDefaultSecret(t)
	tmp, err := os.CreateTemp("", "lightstack-rgwcompat-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString("keystone:\n  rgw_compat_tenant_ids: true\n"); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	cfg, err := LoadConfig(tmp.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Keystone.RGWCompatTenantIDs {
		t.Fatal("keystone.rgw_compat_tenant_ids: true not parsed")
	}
}

// LIGHTSTACK_KEYSTONE_RGW_COMPAT_TENANT_IDS overrides the file value in BOTH
// directions (mirroring the Glance S3 boolean override); unrecognized values
// keep the file value.
func TestLoadConfigKeystoneRGWCompatTenantIDsEnvOverrides(t *testing.T) {
	allowInsecureDefaultSecret(t)
	writeCfg := func(t *testing.T, on bool) string {
		t.Helper()
		tmp, err := os.CreateTemp("", "lightstack-rgwcompatenv-*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(tmp.Name()) })
		if _, err := fmt.Fprintf(tmp, "keystone:\n  rgw_compat_tenant_ids: %t\n", on); err != nil {
			t.Fatal(err)
		}
		tmp.Close()
		return tmp.Name()
	}

	cases := []struct {
		name   string
		file   bool
		env    string
		wantOn bool
	}{
		{"env enables over file off", false, "true", true},
		{"env enables (1) over file off", false, "1", true},
		{"env disables over file on", true, "false", false},
		{"env disables (0) over file on", true, "0", false},
		{"unrecognized value keeps file on", true, "yes-please", true},
		{"empty env keeps file off", false, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LIGHTSTACK_KEYSTONE_RGW_COMPAT_TENANT_IDS", tc.env)
			cfg, err := LoadConfig(writeCfg(t, tc.file))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.Keystone.RGWCompatTenantIDs != tc.wantOn {
				t.Fatalf("rgw_compat_tenant_ids = %v, want %v (file=%v env=%q)", cfg.Keystone.RGWCompatTenantIDs, tc.wantOn, tc.file, tc.env)
			}
		})
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/config.yaml")
	if err == nil {
		t.Error("Expected error for missing config file, got nil")
	}
}

func TestLoadConfigInvalidYAML(t *testing.T) {
	tmpfile, err := os.CreateTemp("", "lightstack-invalid-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	invalidYAML := `
database:
  url: "postgres://test@localhost/test"
  invalid yaml structure
    - broken
`

	if _, err := tmpfile.Write([]byte(invalidYAML)); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
	tmpfile.Close()

	_, err = LoadConfig(tmpfile.Name())
	if err == nil {
		t.Error("Expected error for invalid YAML, got nil")
	}
}

// SPEC-014 T031: the S3/RGW backend config (TLS/CA + timeout) parses from YAML.
func TestLoadConfigGlanceS3(t *testing.T) {
	allowInsecureDefaultSecret(t)
	tmp, err := os.CreateTemp("", "lightstack-s3-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(`
glance:
  storage_mode: s3
  s3_bucket: images
  s3_region: us-east-1
  s3_endpoint: https://rgw.example:443
  s3_ca_bundle: /etc/lightstack/rgw-ca.pem
  s3_insecure_skip_verify: true
  s3_connect_timeout: 3s
`); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	cfg, err := LoadConfig(tmp.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	g := cfg.Glance
	if g.StorageMode != "s3" || g.S3Bucket != "images" || g.S3Region != "us-east-1" ||
		g.S3Endpoint != "https://rgw.example:443" || g.S3CABundle != "/etc/lightstack/rgw-ca.pem" ||
		!g.S3InsecureSkipVerify || g.S3ConnectTimeout != 3*time.Second {
		t.Fatalf("glance S3 config not parsed as expected: %+v", g)
	}
}

// SPEC-014 T031: LIGHTSTACK_GLANCE_S3_* env vars override the file config.
func TestLoadConfigGlanceS3EnvOverrides(t *testing.T) {
	allowInsecureDefaultSecret(t)
	tmp, err := os.CreateTemp("", "lightstack-s3env-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString("glance:\n  storage_mode: s3\n"); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	t.Setenv("LIGHTSTACK_GLANCE_S3_ENDPOINT", "https://env-rgw:443")
	t.Setenv("LIGHTSTACK_GLANCE_S3_BUCKET", "envbucket")
	t.Setenv("LIGHTSTACK_GLANCE_S3_REGION", "eu-west-1")
	t.Setenv("LIGHTSTACK_GLANCE_S3_CA_BUNDLE", "/env/ca.pem")
	t.Setenv("LIGHTSTACK_GLANCE_S3_INSECURE_SKIP_VERIFY", "true")

	cfg, err := LoadConfig(tmp.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	g := cfg.Glance
	if g.S3Endpoint != "https://env-rgw:443" || g.S3Bucket != "envbucket" ||
		g.S3Region != "eu-west-1" || g.S3CABundle != "/env/ca.pem" || !g.S3InsecureSkipVerify {
		t.Fatalf("env overrides not applied: %+v", g)
	}
}

// PR #73 review (r3558640811): the boolean env override must work BOTH ways. A
// base config that ships s3_insecure_skip_verify: true must be disableable via
// LIGHTSTACK_GLANCE_S3_INSECURE_SKIP_VERIFY=false — otherwise TLS verification stays off
// in production with no way to turn it back on from the environment.
func TestLoadConfigGlanceS3InsecureEnvCanDisable(t *testing.T) {
	allowInsecureDefaultSecret(t)
	tmp, err := os.CreateTemp("", "lightstack-s3ins-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	// base config ships skip-verify ENABLED (e.g. a dev image)
	if _, err := tmp.WriteString("glance:\n  storage_mode: s3\n  s3_insecure_skip_verify: true\n"); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	// operator turns it OFF in the pod env
	t.Setenv("LIGHTSTACK_GLANCE_S3_INSECURE_SKIP_VERIFY", "false")

	cfg, err := LoadConfig(tmp.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Glance.S3InsecureSkipVerify {
		t.Fatalf("LIGHTSTACK_GLANCE_S3_INSECURE_SKIP_VERIFY=false was ignored; TLS verification still disabled")
	}
}

// PR #73 review (r3558649609): S3ConnectTimeout must have an env override like every
// other glance.s3.* field, so an operator can tune the fail-fast timeout on a slow RGW
// node without patching a read-only mounted config. A malformed value is ignored (the
// file value stands) rather than silently zeroing the timeout.
func TestLoadConfigGlanceS3ConnectTimeoutEnv(t *testing.T) {
	allowInsecureDefaultSecret(t)
	tmp, err := os.CreateTemp("", "lightstack-s3to-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	// base config ships a 3s timeout
	if _, err := tmp.WriteString("glance:\n  storage_mode: s3\n  s3_connect_timeout: 3s\n"); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	// a valid duration in the env wins over the file
	t.Setenv("LIGHTSTACK_GLANCE_S3_CONNECT_TIMEOUT", "10s")
	cfg, err := LoadConfig(tmp.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Glance.S3ConnectTimeout != 10*time.Second {
		t.Fatalf("valid env override ignored: got %v, want 10s", cfg.Glance.S3ConnectTimeout)
	}

	// a malformed value is ignored; the file's 3s stands (never silently zeroed)
	t.Setenv("LIGHTSTACK_GLANCE_S3_CONNECT_TIMEOUT", "notaduration")
	cfg, err = LoadConfig(tmp.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Glance.S3ConnectTimeout != 3*time.Second {
		t.Fatalf("malformed env value not ignored: got %v, want file value 3s", cfg.Glance.S3ConnectTimeout)
	}
}

// SPEC-021 FR-020: console_proxy_base_url is an ORIGIN. Lightstack appends the client
// page and the token query itself, so anything more than scheme://host[:port] would
// produce a console link that 404s — e.g. ".../vnc_auto.html/vnc_lite.html?path=...".
// That is exactly the value upstream Nova's novncproxy_base_url takes, so it must be
// refused at load rather than silently accepted or rewritten.
func TestLoadConfigConsoleProxyBaseURL(t *testing.T) {
	allowInsecureDefaultSecret(t)
	cases := []struct {
		value     string
		wantErr   bool
		wantInErr string // the corrected origin, when there is one to suggest
		notInErr  string // the error lands in startup logs: no credentials
	}{
		{value: "", wantErr: false}, // empty derives the origin from the request (FR-021)
		{value: "http://10.2.199.101:6080", wantErr: false},
		{value: "https://console.example.com", wantErr: false},
		{value: "http://10.2.199.101:6080/", wantErr: false}, // the trailing slash is trimmed
		{value: "http://[2001:db8::1]:6080", wantErr: false},

		{value: "http://10.2.199.101:6080/vnc_auto.html", wantErr: true, wantInErr: `"http://10.2.199.101:6080"`},
		{value: "http://10.2.199.101:6080/vnc_lite.html", wantErr: true, wantInErr: `"http://10.2.199.101:6080"`},
		{value: "http://10.2.199.101:6080?token=x", wantErr: true},
		{value: "http://10.2.199.101:6080#top", wantErr: true},
		{value: "http://user:secret@10.2.199.101:6080", wantErr: true, notInErr: "secret"},
		{value: "10.2.199.101:6080", wantErr: true},
		{value: "ftp://10.2.199.101:6080", wantErr: true},
		{value: "http://", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lightstack.yaml")
			content := "nova:\n  console_proxy_base_url: \"" + tc.value + "\"\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, err := LoadConfig(path)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("LoadConfig rejected a valid origin: %v", err)
				}
				// Accepted means used as written — never rewritten into something else.
				if cfg.Nova.ConsoleProxyBaseURL != tc.value {
					t.Fatalf("ConsoleProxyBaseURL = %q, want %q unchanged", cfg.Nova.ConsoleProxyBaseURL, tc.value)
				}
				return
			}
			if err == nil {
				t.Fatalf("LoadConfig accepted %q; it must be refused, the console link would not resolve", tc.value)
			}
			if !strings.Contains(err.Error(), "console_proxy_base_url") {
				t.Errorf("error does not name the key: %v", err)
			}
			if tc.wantInErr != "" && !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("error does not suggest the origin %s: %v", tc.wantInErr, err)
			}
			if tc.notInErr != "" && strings.Contains(err.Error(), tc.notInErr) {
				t.Errorf("error leaks %q: %v", tc.notInErr, err)
			}
		})
	}
}

// state_path is the node-local root (pkg/paths). It must be absolute: libvirtd on the
// host opens the paths Lightstack writes into the domain XML, so both sides must read
// the same string, and a relative root has no meaning on the host. "/" is refused too —
// cleanup purges the per-service subdirectories directly under it.
func TestLoadConfigStatePath(t *testing.T) {
	allowInsecureDefaultSecret(t)
	cases := []struct {
		name, yaml, want string
		wantErr          bool
	}{
		{name: "omitted defaults", yaml: "", want: "/var/lib/lightstack"},
		{name: "custom", yaml: "state_path: /data/lightstack\n", want: "/data/lightstack"},
		{name: "trailing slash cleaned", yaml: "state_path: /data/lightstack/\n", want: "/data/lightstack"},
		{name: "relative refused", yaml: "state_path: data/lightstack\n", wantErr: true},
		{name: "root refused", yaml: "state_path: /\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lightstack.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, err := LoadConfig(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("LoadConfig accepted %q; it must be refused", tc.yaml)
				}
				if !strings.Contains(err.Error(), "state_path") {
					t.Errorf("error does not name the key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.StatePath != tc.want {
				t.Errorf("StatePath = %q; want %q", cfg.StatePath, tc.want)
			}
		})
	}
}

// In real mode every guest's console socket is <state_path>/console-sockets/<uuid>.sock,
// and QEMU refuses a socket path longer than 108 bytes, so a longer root would stop every
// guest from starting. Stub mode creates no sockets and is not limited (SPEC-021 FR-017).
func TestLoadConfigStatePathLengthInRealMode(t *testing.T) {
	allowInsecureDefaultSecret(t)
	root50 := "/" + strings.Repeat("a", 49)
	root51 := "/" + strings.Repeat("a", 50)
	for _, tc := range []struct {
		name, mode, root string
		wantErr          bool
	}{
		{"real, 50 characters", "real", root50, false},
		{"real, 51 characters", "real", root51, true},
		{"stub, 51 characters", "stub", root51, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lightstack.yaml")
			yaml := "state_path: " + tc.root + "\nnova:\n  libvirt_mode: " + tc.mode + "\n"
			if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("LoadConfig: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("LoadConfig accepted a state_path too long for a console socket path")
			}
			if !strings.Contains(err.Error(), "state_path") || !strings.Contains(err.Error(), "50") {
				t.Errorf("error does not name the key and its limit: %v", err)
			}
		})
	}
}

// console_enabled says whether a console proxy is deployed at all (SPEC-021). Omitted
// means yes — the deploy scripts run one and never set the key — and only an explicit
// false turns consoles off, which is what the chart renders wherever it deploys no
// proxy (multi-node, or consoleProxy.enabled=false).
func TestLoadConfigConsoleEnabled(t *testing.T) {
	allowInsecureDefaultSecret(t)
	for _, tc := range []struct {
		name, yaml string
		want       bool
	}{
		{name: "omitted", yaml: "", want: true},
		{name: "true", yaml: "nova:\n  console_enabled: true\n", want: true},
		{name: "false", yaml: "nova:\n  console_enabled: false\n", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lightstack.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if got := cfg.Nova.ConsolesEnabled(); got != tc.want {
				t.Errorf("ConsolesEnabled() = %v; want %v", got, tc.want)
			}
		})
	}
}

// The console token TTL and reconcile interval fall back to their defaults when the file
// omits them or sets zero or a negative value (SPEC-021). LoadConfig is the only guard:
// the interval goes straight to time.NewTicker in the token reconciler, which panics on
// a non-positive duration, and a zero TTL would issue tokens that are already expired.
func TestLoadConfigConsoleDurations(t *testing.T) {
	allowInsecureDefaultSecret(t)
	for _, tc := range []struct {
		name, yaml    string
		ttl, interval time.Duration
	}{
		{"omitted", "", DefaultConsoleTokenTTL, DefaultConsoleReconcileInterval},
		{"zero", "nova:\n  console_token_ttl: 0s\n  console_reconcile_interval: 0s\n",
			DefaultConsoleTokenTTL, DefaultConsoleReconcileInterval},
		{"negative", "nova:\n  console_token_ttl: -1m\n  console_reconcile_interval: -5s\n",
			DefaultConsoleTokenTTL, DefaultConsoleReconcileInterval},
		{"set", "nova:\n  console_token_ttl: 2m\n  console_reconcile_interval: 5s\n",
			2 * time.Minute, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lightstack.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.Nova.ConsoleTokenTTL != tc.ttl {
				t.Errorf("ConsoleTokenTTL = %v; want %v", cfg.Nova.ConsoleTokenTTL, tc.ttl)
			}
			if cfg.Nova.ConsoleReconcileInterval != tc.interval {
				t.Errorf("ConsoleReconcileInterval = %v; want %v", cfg.Nova.ConsoleReconcileInterval, tc.interval)
			}
		})
	}
}

// metadata.trust_instance_header defaults off, since with it on any guest can read any
// instance's metadata and user-data. Test stacks turn it on from the environment, and
// the override works both ways, like every other boolean one.
func TestLoadConfigMetadataTrustInstanceHeader(t *testing.T) {
	allowInsecureDefaultSecret(t)
	for _, tc := range []struct {
		name, file, env string
		want            bool
	}{
		{"off by default", "metadata:\n  bind_address: \"0.0.0.0:8775\"\n", "", false},
		{"on from the file", "metadata:\n  trust_instance_header: true\n", "", true},
		{"on from the environment", "metadata: {}\n", "true", true},
		{"off from the environment", "metadata:\n  trust_instance_header: true\n", "false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lightstack.yaml")
			if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("LIGHTSTACK_METADATA_TRUST_INSTANCE_HEADER", tc.env)

			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.Metadata.TrustInstanceHeader != tc.want {
				t.Errorf("TrustInstanceHeader = %v; want %v", cfg.Metadata.TrustInstanceHeader, tc.want)
			}
		})
	}
}

// SPEC-025 T008 (FR-024d, FR-042b): the two compute settings this feature adds must
// have working defaults and must clamp a non-positive value, on the same principle as
// the console settings — a deployment that never heard of either still reconciles
// drift and still bounds its audit growth.
func TestLoadConfigComputeIntervalDefaults(t *testing.T) {
	allowInsecureDefaultSecret(t)
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "lightstack.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("omitted entirely falls back to the defaults", func(t *testing.T) {
		cfg, err := LoadConfig(write(t, "nova:\n  libvirt_mode: real\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Nova.StateReconcileInterval != DefaultStateReconcileInterval {
			t.Errorf("StateReconcileInterval = %v; want %v",
				cfg.Nova.StateReconcileInterval, DefaultStateReconcileInterval)
		}
		if cfg.Nova.InstanceActionRetention != DefaultInstanceActionRetention {
			t.Errorf("InstanceActionRetention = %v; want %v",
				cfg.Nova.InstanceActionRetention, DefaultInstanceActionRetention)
		}
	})

	t.Run("explicit values are honored", func(t *testing.T) {
		cfg, err := LoadConfig(write(t, `
nova:
  state_reconcile_interval: 5s
  instance_action_retention: 720h
`))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Nova.StateReconcileInterval != 5*time.Second {
			t.Errorf("StateReconcileInterval = %v; want 5s", cfg.Nova.StateReconcileInterval)
		}
		if cfg.Nova.InstanceActionRetention != 720*time.Hour {
			t.Errorf("InstanceActionRetention = %v; want 720h", cfg.Nova.InstanceActionRetention)
		}
	})

	// Zero would mean a busy loop for the interval and "reclaim everything
	// immediately" for the retention age. Neither is a plausible reading of 0, so
	// both are clamped rather than honored.
	t.Run("non-positive values are clamped, not accepted", func(t *testing.T) {
		for _, body := range []string{
			"nova:\n  state_reconcile_interval: 0s\n  instance_action_retention: 0s\n",
			"nova:\n  state_reconcile_interval: -1s\n  instance_action_retention: -1h\n",
		} {
			cfg, err := LoadConfig(write(t, body))
			if err != nil {
				t.Fatalf("LoadConfig(%q): %v", body, err)
			}
			if cfg.Nova.StateReconcileInterval != DefaultStateReconcileInterval {
				t.Errorf("StateReconcileInterval = %v; want the default %v",
					cfg.Nova.StateReconcileInterval, DefaultStateReconcileInterval)
			}
			if cfg.Nova.InstanceActionRetention != DefaultInstanceActionRetention {
				t.Errorf("InstanceActionRetention = %v; want the default %v",
					cfg.Nova.InstanceActionRetention, DefaultInstanceActionRetention)
			}
		}
	})

	// The retention age must be long enough to be an audit trail, not a cache.
	t.Run("the default retention is a plausible audit period", func(t *testing.T) {
		if DefaultInstanceActionRetention < 30*24*time.Hour {
			t.Errorf("DefaultInstanceActionRetention = %v; too short to be an audit trail",
				DefaultInstanceActionRetention)
		}
	})
}

// A SYNCHRONOUS stop holds the HTTP request open for as long as it waits for the guest,
// so the grace period it is allowed is bounded by the server's write deadline — not by
// what upstream's asynchronous equivalent can afford.
//
// This was found on a host, the expensive way. With the grace period set to upstream's
// 60s default and HTTPWriteTimeout also 60s, the forced-power-off path reached its
// response write at or after the deadline every single time, so Go closed the connection
// unanswered: `RemoteDisconnected('Remote end closed connection without response')`. The
// stop had worked; it just could never say so. Which is the exact failure class the
// feature exists to remove, reintroduced one layer down.
func TestShutdownTimeoutFitsInsideTheWriteDeadline(t *testing.T) {
	allowInsecureDefaultSecret(t)
	// The invariant, as arithmetic on the constants. This is the assertion that would
	// have caught the defect before it reached a host: a default that cannot answer is
	// a default that is wrong, and nothing else in the suite compares these two.
	t.Run("the default leaves room to answer", func(t *testing.T) {
		if DefaultShutdownTimeout+LifecycleResponseSlack > HTTPWriteTimeout {
			t.Errorf("DefaultShutdownTimeout(%v) + LifecycleResponseSlack(%v) > HTTPWriteTimeout(%v); "+
				"a stop that runs the full grace period cannot write its own response",
				DefaultShutdownTimeout, LifecycleResponseSlack, HTTPWriteTimeout)
		}
	})

	t.Run("the ceiling leaves room to answer", func(t *testing.T) {
		if MaxShutdownTimeout+LifecycleResponseSlack > HTTPWriteTimeout {
			t.Errorf("MaxShutdownTimeout(%v) + LifecycleResponseSlack(%v) > HTTPWriteTimeout(%v)",
				MaxShutdownTimeout, LifecycleResponseSlack, HTTPWriteTimeout)
		}
	})

	// A blocking stop is also bounded by whatever sits in front of Lightstack. 30s is the
	// default read timeout of both gunicorn (Horizon) and a good many ingresses, so a
	// default above it would fail in a deployment rather than in a test.
	t.Run("the default stays inside a common proxy timeout", func(t *testing.T) {
		if DefaultShutdownTimeout > 30*time.Second {
			t.Errorf("DefaultShutdownTimeout = %v; a synchronous stop above 30s trips "+
				"gunicorn's and most ingresses' default read timeout", DefaultShutdownTimeout)
		}
	})

	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "lightstack.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("omitted falls back to the default", func(t *testing.T) {
		cfg, err := LoadConfig(write(t, "nova:\n  libvirt_mode: real\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Nova.ShutdownTimeout != DefaultShutdownTimeout {
			t.Errorf("ShutdownTimeout = %v; want %v", cfg.Nova.ShutdownTimeout, DefaultShutdownTimeout)
		}
	})

	t.Run("an explicit value inside the ceiling is honored", func(t *testing.T) {
		cfg, err := LoadConfig(write(t, "nova:\n  shutdown_timeout: 15s\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Nova.ShutdownTimeout != 15*time.Second {
			t.Errorf("ShutdownTimeout = %v; want 15s", cfg.Nova.ShutdownTimeout)
		}
	})

	// Clamped rather than refused: an operator who asks for a longer grace than a
	// synchronous stop can deliver has made a reasonable request of an implementation
	// that cannot honour it. Refusing to start over it would be worse than shortening it
	// and saying so.
	t.Run("a value above the ceiling is clamped", func(t *testing.T) {
		cfg, err := LoadConfig(write(t, "nova:\n  shutdown_timeout: 10m\n"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Nova.ShutdownTimeout != MaxShutdownTimeout {
			t.Errorf("ShutdownTimeout = %v; want it clamped to %v — a grace period the "+
				"handler cannot answer within is one that loses its response",
				cfg.Nova.ShutdownTimeout, MaxShutdownTimeout)
		}
	})

	t.Run("non-positive falls back to the default", func(t *testing.T) {
		for _, body := range []string{"nova:\n  shutdown_timeout: 0s\n", "nova:\n  shutdown_timeout: -5s\n"} {
			cfg, err := LoadConfig(write(t, body))
			if err != nil {
				t.Fatalf("LoadConfig(%q): %v", body, err)
			}
			if cfg.Nova.ShutdownTimeout != DefaultShutdownTimeout {
				t.Errorf("ShutdownTimeout for %q = %v; want the default %v",
					body, cfg.Nova.ShutdownTimeout, DefaultShutdownTimeout)
			}
		}
	})
}

// writeTestConfig persists body to a temp config file and returns its path.
func writeTestConfig(t *testing.T, body string) string {
	t.Helper()
	tmp, err := os.CreateTemp("", "lightstack-cfg-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(tmp.Name()) })
	if _, err := tmp.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	return tmp.Name()
}

// hermeticGuardEnv neutralizes secret-related environment a developer shell
// may carry (LIGHTSTACK_JWT_SECRET, the opt-out hatch), so the guard tests
// observe the config file and nothing else.
func hermeticGuardEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LIGHTSTACK_JWT_SECRET", "")
	t.Setenv("LIGHTSTACK_ALLOW_INSECURE_DEFAULT_SECRET", "")
}

// TestLoadConfigRefusesInsecureDefaultJWTSecret pins the S2 guard: the secrets
// shipped in this repository's config and examples are public, and tokens are
// stateless — knowing one is enough to mint an admin token for every service.
// Case variants and surrounding whitespace are refused too: the uppercase
// spelling shipped in deployments/README-KVM.md is the same placeholder.
func TestLoadConfigRefusesInsecureDefaultJWTSecret(t *testing.T) {
	hermeticGuardEnv(t)
	for _, secret := range []string{
		"",                        // omitted from the file
		"change-me-in-production", // config/lightstack.yaml
		"change-me-in-production-use-openssl-rand-base64-32", // .env.example
		"CHANGE-ME-IN-PRODUCTION",                            // deployments/README-KVM.md
		"  change-me-in-production  ",
		"change-this-in-production-min-32-chars-required-for-security", // docs/SINGLE_NODE_DEPLOYMENT.md
		"production-secret-min-32-chars-change-this-immediately",       // docs/SCALING.md
	} {
		body := "keystone:\n  jwt_secret: \"" + secret + "\"\n"
		_, err := LoadConfig(writeTestConfig(t, body))
		if err == nil {
			t.Errorf("LoadConfig accepted jwt_secret %q; want refusal", secret)
			continue
		}
		if !strings.Contains(err.Error(), "refusing to start") {
			t.Errorf("LoadConfig(%q) error does not explain the refusal: %v", secret, err)
		}
	}
}

// TestLoadConfigRefusesInsecureDefaultAdminPassword pins the admin half of
// the guard: config/lightstack.yaml ships admin_password "secret", which is
// public and authenticates as the bootstrap admin directly. Empty is refused
// whenever an admin account is configured (the seeded account would accept an
// empty password); a config that omits both admin fields seeds no account and
// must keep loading (the minimal fixtures above the guard tests rely on it).
func TestLoadConfigRefusesInsecureDefaultAdminPassword(t *testing.T) {
	hermeticGuardEnv(t)
	const goodSecret = "k4FNxqKmLwW3ZRFvMscJ0d1oO5gYhN"
	for _, tc := range []struct {
		name     string
		password string
	}{
		{"shipped default", "secret"},
		{"uppercase default", "SECRET"},
		{"padded default", "  secret  "},
		{"empty with a user", ""},
		{"README-KVM placeholder", "PASTE-OUTPUT-OF-openssl-rand-base64-24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "keystone:\n  jwt_secret: \"" + goodSecret + "\"\n  admin_user: admin\n  admin_password: \"" + tc.password + "\"\n"
			_, err := LoadConfig(writeTestConfig(t, body))
			if err == nil {
				t.Fatalf("LoadConfig accepted admin_password %q; want refusal", tc.password)
			}
			if !strings.Contains(err.Error(), "admin_password") {
				t.Errorf("error does not name admin_password: %v", err)
			}
		})
	}

	// Both admin fields omitted: REFUSED. migrations/002 seeds an enabled
	// admin/secret account in every deployment and BootstrapAdmin only warns
	// on an empty configured password — omitting the field is how the public
	// credential stays live (round-2 review F1).
	if _, err := LoadConfig(writeTestConfig(t, "keystone:\n  jwt_secret: \""+goodSecret+"\"\n")); err == nil {
		t.Error("LoadConfig without admin fields: accepted; want refusal (the seeded admin/secret stays live)")
	} else if !strings.Contains(err.Error(), "admin_password") {
		t.Errorf("error does not name admin_password: %v", err)
	}

	// A random password with a random secret: the positive control.
	body := "keystone:\n  jwt_secret: \"" + goodSecret + "\"\n  admin_user: admin\n  admin_password: \"T3st-Not-The-Shipped-Default\"\n"
	cfg, err := LoadConfig(writeTestConfig(t, body))
	if err != nil {
		t.Fatalf("LoadConfig with a random admin password: %v", err)
	}
	if cfg.Keystone.AdminPassword != "T3st-Not-The-Shipped-Default" {
		t.Errorf("AdminPassword = %q; want it preserved", cfg.Keystone.AdminPassword)
	}
}

// TestLoadConfigAllowsInsecureDefaultSecretWithOverride pins the documented
// escape hatch for isolated development and test stacks — for both halves of
// the guard.
func TestLoadConfigAllowsInsecureDefaultSecretWithOverride(t *testing.T) {
	hermeticGuardEnv(t) // a developer shell's LIGHTSTACK_JWT_SECRET must not leak in
	for _, opt := range []string{"1", "true"} {
		t.Run("opt="+opt, func(t *testing.T) {
			t.Setenv("LIGHTSTACK_ALLOW_INSECURE_DEFAULT_SECRET", opt)
			cfg, err := LoadConfig(writeTestConfig(t,
				"keystone:\n  jwt_secret: \"change-me-in-production\"\n  admin_user: admin\n  admin_password: \"secret\"\n"))
			if err != nil {
				t.Fatalf("LoadConfig with override %q: %v", opt, err)
			}
			if cfg.Keystone.JWTSecret != "change-me-in-production" {
				t.Errorf("JWTSecret = %q; want it passed through unchanged", cfg.Keystone.JWTSecret)
			}
			if cfg.Keystone.AdminPassword != "secret" {
				t.Errorf("AdminPassword = %q; want it passed through unchanged", cfg.Keystone.AdminPassword)
			}
		})
	}
}

// TestLoadConfigAcceptsARandomSecret is the positive control for the guard: a
// non-default secret (and non-default admin password) loads without any
// opt-in.
func TestLoadConfigAcceptsARandomSecret(t *testing.T) {
	hermeticGuardEnv(t)
	cfg, err := LoadConfig(writeTestConfig(t, "keystone:\n  jwt_secret: \"k4FNxqKmLwW3ZRFvMscJ0d1oO5gYhN\"\n  admin_password: \"T3st-Not-The-Shipped-Default\"\n"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Keystone.JWTSecret != "k4FNxqKmLwW3ZRFvMscJ0d1oO5gYhN" {
		t.Errorf("JWTSecret = %q; want it preserved", cfg.Keystone.JWTSecret)
	}
}

// TestLoadConfigForMigrationSkipsSecretGuard pins the migration tool's
// exemption: it never signs or validates a token, and shipped deployment
// paths (image entrypoint, Kubernetes init container) run it against the
// canned config before any secret is wired in.
func TestLoadConfigForMigrationSkipsSecretGuard(t *testing.T) {
	hermeticGuardEnv(t)
	cfg, err := LoadConfigForMigration(writeTestConfig(t, "database:\n  url: postgres://u:p@h/l\n"))
	if err != nil {
		t.Fatalf("LoadConfigForMigration: %v", err)
	}
	if cfg.Database.URL != "postgres://u:p@h/l" {
		t.Errorf("Database.URL = %q; want it preserved", cfg.Database.URL)
	}
}

// TestLoadConfigAdminPasswordEnvOverride pins the container path: the image
// ships the canned config (admin_password "secret"), so `docker run` injects
// the real password through the environment like the JWT secret.
func TestLoadConfigAdminPasswordEnvOverride(t *testing.T) {
	hermeticGuardEnv(t)
	t.Setenv("LIGHTSTACK_ADMIN_PASSWORD", "injected-through-env")
	body := "keystone:\n  jwt_secret: \"k4FNxqKmLwW3ZRFvMscJ0d1oO5gYhN\"\n  admin_user: admin\n  admin_password: \"secret\"\n"
	cfg, err := LoadConfig(writeTestConfig(t, body))
	if err != nil {
		t.Fatalf("LoadConfig with LIGHTSTACK_ADMIN_PASSWORD: %v", err)
	}
	if cfg.Keystone.AdminPassword != "injected-through-env" {
		t.Errorf("AdminPassword = %q; want the env value", cfg.Keystone.AdminPassword)
	}
}

// TestLoadConfigForCLISkipsSecretGuard pins the CLI subcommands' exemption
// (cleanup, console-tokens, migrate): they run on the same host as the
// server, whose config is only valid because the server's own process
// carries the escape hatch — the guard must not refuse them there.
func TestLoadConfigForCLISkipsSecretGuard(t *testing.T) {
	hermeticGuardEnv(t)
	body := "database:\n  url: postgres://u:p@h/l\nkeystone:\n  jwt_secret: \"change-me-in-production\"\n  admin_user: admin\n  admin_password: \"secret\"\n"
	cfg, err := LoadConfigForCLI(writeTestConfig(t, body))
	if err != nil {
		t.Fatalf("LoadConfigForCLI: %v", err)
	}
	if cfg.Keystone.JWTSecret != "change-me-in-production" {
		t.Errorf("JWTSecret = %q; want it preserved", cfg.Keystone.JWTSecret)
	}
}
