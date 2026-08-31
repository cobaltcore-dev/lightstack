package cinder

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestSerializeVolume: the volume serializer emits every field Horizon's volume
// table dereferences, each non-null, matching the upstream Cinder v3 shape (F9,
// FR-001/004). A row rendered with these fields present is what stops Horizon's
// get_rows TemplateSyntaxError. No DB required — serializeVolume is pure.
func TestSerializeVolume(t *testing.T) {
	now := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)

	// Volume created with no explicit type → volume_type defaults to __DEFAULT__.
	v := serializeVolume(volumeRow{
		ID: "id1", Name: "vol1", Description: "desc1", TenantID: "proj1", UserID: "user1",
		Status: "available", Size: 5, Bootable: true, CreatedAt: now, UpdatedAt: now,
	})

	requiredNonNull := []string{
		"id", "name", "description", "tenant_id", "user_id", "size", "status", "bootable",
		"created_at", "updated_at", "attachments", "metadata",
		"volume_type", "volume_type_id", "availability_zone", "encrypted", "multiattach",
		"consumes_quota", "shared_targets",
		"os-vol-tenant-attr:tenant_id", "os-vol-host-attr:host",
	}
	for _, k := range requiredNonNull {
		if val, ok := v[k]; !ok || val == nil {
			t.Errorf("field %q missing or nil (Horizon renders it)", k)
		}
	}

	if v["volume_type"] != defaultVolumeType {
		t.Errorf("default volume_type = %v, want %q", v["volume_type"], defaultVolumeType)
	}
	if v["availability_zone"] != "nova" {
		t.Errorf("availability_zone = %v, want nova", v["availability_zone"])
	}
	if v["encrypted"] != false {
		t.Errorf("encrypted = %v, want false", v["encrypted"])
	}
	if v["multiattach"] != false {
		t.Errorf("multiattach = %v, want false", v["multiattach"])
	}
	if v["bootable"] != "true" {
		t.Errorf("bootable = %v, want string \"true\"", v["bootable"])
	}
	if v["os-vol-tenant-attr:tenant_id"] != "proj1" {
		t.Errorf("os-vol-tenant-attr:tenant_id = %v, want proj1", v["os-vol-tenant-attr:tenant_id"])
	}

	// Fields upstream Cinder models but Lightstack does not: the key must be PRESENT (a client
	// dereferencing a missing key raises) while carrying upstream's own null value.
	for _, k := range []string{"consistencygroup_id", "migration_status", "replication_status", "snapshot_id", "source_volid"} {
		val, ok := v[k]
		if !ok {
			t.Errorf("field %q missing; upstream Cinder always emits it (as null)", k)
		}
		if val != nil {
			t.Errorf("field %q = %v, want nil to match upstream", k, val)
		}
	}
	if v["description"] != "desc1" {
		t.Errorf("description = %v, want desc1 (real column, migration 012)", v["description"])
	}

	// An explicit volume type is read back verbatim (FR-003), and volume_type_id agrees.
	v2 := serializeVolume(volumeRow{
		ID: "id2", Name: "vol2", TenantID: "p", UserID: "u", VolumeType: "gold",
		Status: "available", Size: 1, CreatedAt: now, UpdatedAt: now,
	})
	if v2["volume_type"] != "gold" {
		t.Errorf("explicit volume_type = %v, want gold", v2["volume_type"])
	}
	if v2["volume_type_id"] != v2["volume_type"] {
		t.Errorf("volume_type_id = %v, want it to agree with volume_type (%v)", v2["volume_type_id"], v2["volume_type"])
	}
	if v2["bootable"] != "false" {
		t.Errorf("bootable = %v, want string \"false\"", v2["bootable"])
	}
}

// TestGetDefaultVolumeType: GET /v3/types/default reports __DEFAULT__ so it agrees
// with the volume_type a volume reads back (F9, spec Q2). No DB required.
func TestGetDefaultVolumeType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v3/types/default", nil)

	(&Service{}).GetDefaultVolumeType(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		VolumeType struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"volume_type"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.VolumeType.Name != defaultVolumeType || resp.VolumeType.ID != defaultVolumeType {
		t.Errorf("types/default = {id:%q,name:%q}, want both %q", resp.VolumeType.ID, resp.VolumeType.Name, defaultVolumeType)
	}
}

// SPEC-025 T013 (FR-007a, FR-007b): the block-storage v3 version document must derive
// its self href from the request.
//
// It was hardcoded to "http://lightstack:8776/v3/" — a hostname that resolves only inside the
// compose file's network. Every client outside it, including the dashboard and the
// infrastructure-as-code provider, is handed an address it cannot reach. Same
// discovery defect as the compute version document (B4), one service over.
func TestCinderVersionDocumentSelfLinkIsDerived(t *testing.T) {
	gin.SetMode(gin.TestMode)

	svc := &Service{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v3", nil)
	c.Request.Host = "lightstack.example:8776"

	svc.GetVersionV3(c)

	var doc struct {
		Version struct {
			Links []struct {
				Rel  string `json:"rel"`
				HRef string `json:"href"`
			} `json:"links"`
		} `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decoding version document: %v (body: %s)", err, w.Body.String())
	}
	if len(doc.Version.Links) == 0 {
		t.Fatal("version document has no links")
	}

	for _, l := range doc.Version.Links {
		if strings.Contains(l.HRef, "://lightstack:") || strings.Contains(l.HRef, "://lightstack/") {
			t.Errorf("href %q names the compose-file hostname; no real client can resolve it", l.HRef)
		}
		u, err := url.Parse(l.HRef)
		if err != nil {
			t.Fatalf("href %q does not parse: %v", l.HRef, err)
		}
		if u.Scheme == "" || u.Host == "" {
			t.Errorf("href %q is not absolute", l.HRef)
		}
		if u.Host != "lightstack.example:8776" {
			t.Errorf("href %q does not name the host the client addressed", l.HRef)
		}
	}
}
