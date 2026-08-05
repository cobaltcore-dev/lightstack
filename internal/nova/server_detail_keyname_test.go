package nova

import (
	"net/http"
	"testing"
)

// US1 (FR-001/FR-002/FR-003): key_name is persisted at create and read back on
// server show + list-detail; null when the instance has no keypair.

func TestServerDetail_KeyName_RoundTrip(t *testing.T) {
	requireDB(t)
	cleanupSpec017(t)
	defer cleanupSpec017(t)
	createTestKeypair(t, "spec017-key")

	r := newNovaTestRouter()
	code, body := createServerAs(t, r, seededProjectID, seededUserID,
		`{"server":{"name":"spec017-kn","flavorRef":"m1.small","key_name":"spec017-key"}}`)
	if code != http.StatusAccepted {
		t.Fatalf("create: code=%d body=%s; want 202", code, body)
	}
	id, _ := serverBody(t, body)["id"].(string)
	if id == "" {
		t.Fatalf("create: no id in body=%s", body)
	}

	// show
	code, body = doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
	if got := serverBody(t, body)["key_name"]; got != "spec017-key" {
		t.Errorf("show key_name=%v; want spec017-key", got)
	}

	// list-detail
	code, body = doReq(r, http.MethodGet, "/v2.1/servers/detail", "", testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("detail: code=%d body=%s", code, body)
	}
	found := false
	for _, s := range serversBody(t, body) {
		if s["id"] == id {
			found = true
			if s["key_name"] != "spec017-key" {
				t.Errorf("detail key_name=%v; want spec017-key", s["key_name"])
			}
			// tenant_id comes from the scanned row, not the request context — a
			// wrong scan target would silently emit "" for every server.
			if s["tenant_id"] != seededProjectID {
				t.Errorf("detail tenant_id=%v; want %s", s["tenant_id"], seededProjectID)
			}
		}
	}
	if !found {
		t.Errorf("detail: server %s not found in list", id)
	}
}

func TestServerDetail_KeyName_NullWhenAbsent(t *testing.T) {
	requireDB(t)
	cleanupSpec017(t)
	defer cleanupSpec017(t)

	r := newNovaTestRouter()
	code, body := createServerAs(t, r, seededProjectID, seededUserID,
		`{"server":{"name":"spec017-nokey","flavorRef":"m1.small"}}`)
	if code != http.StatusAccepted {
		t.Fatalf("create: code=%d body=%s; want 202", code, body)
	}
	id, _ := serverBody(t, body)["id"].(string)

	code, body = doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
	s := serverBody(t, body)
	v, ok := s["key_name"]
	if !ok {
		t.Errorf("show: key_name field absent; want present with null value. body=%s", body)
	}
	if v != nil {
		t.Errorf("show key_name=%v; want null", v)
	}
}

// FR-002a: an unknown key_name is rejected synchronously with 400 and no instance
// is created.
func TestCreateServer_UnknownKeyName_400(t *testing.T) {
	requireDB(t)
	cleanupSpec017(t)
	defer cleanupSpec017(t)

	r := newNovaTestRouter()
	code, body := createServerAs(t, r, seededProjectID, seededUserID,
		`{"server":{"name":"spec017-badkey","flavorRef":"m1.small","key_name":"spec017-nope"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("create with unknown key_name: code=%d body=%s; want 400", code, body)
	}
	if n := instanceCount(t, "spec017-badkey", seededProjectID); n != 0 {
		t.Errorf("instance created despite 400: count=%d", n)
	}
}
