package common

import (
	"fmt"
	"os"
	"testing"
	"time"
)

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
  admin_password: "secret"

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
