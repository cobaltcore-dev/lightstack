package nova

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An instance has no libvirt domain until a real-mode create gives it one: every
// stub-mode instance, and every real one whose create failed, carries a NULL
// libvirt_domain_id. Six handlers read that column into a plain string, pgx refuses
// to scan NULL into one, and each answered "500 database error" for exactly those
// instances. These pin that they now get past the read.

func setInstanceStatus(t *testing.T, id, status string) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(),
		`UPDATE instances SET status = $2, flavor_id = (SELECT id FROM flavors WHERE name = 'm1.small')
		  WHERE id = $1`, id, status); err != nil {
		t.Fatalf("setting instance status: %v", err)
	}
}

func instanceStatus(t *testing.T, id string) string {
	t.Helper()
	var status string
	if err := testPool.QueryRow(t.Context(), "SELECT status FROM instances WHERE id = $1", id).Scan(&status); err != nil {
		t.Fatalf("reading instance status: %v", err)
	}
	return status
}

func TestLifecycleActionsWorkWithoutADomain(t *testing.T) {
	requireDB(t)

	for _, tc := range []struct{ name, from, body, want string }{
		{"suspend", "ACTIVE", `{"suspend":null}`, "SUSPENDED"},
		{"resume", "SUSPENDED", `{"resume":null}`, "ACTIVE"},
		{"shelve", "ACTIVE", `{"shelve":null}`, "SHELVED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNovaTestRouter()
			id := seedConsoleInstance(t, seededProjectID)
			setInstanceStatus(t, id, tc.from)
			// shelve records a snapshot row with no foreign key to the instance.
			t.Cleanup(func() {
				_, _ = testPool.Exec(context.Background(), "DELETE FROM instance_snapshots WHERE instance_id = $1", id)
			})

			code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", tc.body,
				testAuthHdr(seededProjectID, seededUserID))

			if code != http.StatusAccepted {
				t.Fatalf("%s: status = %d; want 202. body: %s", tc.name, code, body)
			}
			if got := instanceStatus(t, id); got != tc.want {
				t.Errorf("%s: instance status = %q; want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestVolumeAttachAndDetachWithoutADomain(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	id := seedConsoleInstance(t, seededProjectID)

	var volumeID string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO volumes (name, project_id, user_id, size_gb, status)
		 VALUES ('null-domain-volume', $1, $2, 1, 'available') RETURNING id`,
		seededProjectID, seededUserID).Scan(&volumeID); err != nil {
		t.Fatalf("seeding volume: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM volume_attachments WHERE volume_id = $1", volumeID)
		_, _ = testPool.Exec(context.Background(), "DELETE FROM volumes WHERE id = $1", volumeID)
	})
	hdr := testAuthHdr(seededProjectID, seededUserID)

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/os-volume_attachments",
		fmt.Sprintf(`{"volumeAttachment":{"volumeId":%q}}`, volumeID), hdr)
	if code != http.StatusOK {
		t.Fatalf("attach: status = %d; want 200. body: %s", code, body)
	}

	code, body = doReq(r, http.MethodDelete, "/v2.1/servers/"+id+"/os-volume_attachments/"+volumeID, "", hdr)
	if code != http.StatusAccepted {
		t.Fatalf("detach: status = %d; want 202. body: %s", code, body)
	}
}

// Interface attach needs Neutron to go further than this, so the assertion is the
// narrow one: the handler reaches its own port lookup (404 for an unknown port)
// instead of failing on the instance read.
func TestInterfaceAttachGetsPastTheInstanceReadWithoutADomain(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	id := seedConsoleInstance(t, seededProjectID)

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/os-interface",
		`{"interfaceAttachment":{"port_id":"00000000-0000-0000-0000-00000000dead"}}`,
		testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 for an unknown port. body: %s", code, body)
	}
}

// The attach handlers used to check only ErrNoRows after reading the instance, so any
// other failure — the NULL scan above, or the database going away — was swallowed and
// the handler carried on with zero values. A cancelled request context stands in for
// the outage: the read must fail the request, not be ignored.
func TestVolumeAttachFailsOnAnInstanceReadError(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v2.1/servers/00000000-0000-0000-0000-00000000beef/os-volume_attachments",
		strings.NewReader(`{"volumeAttachment":{"volumeId":"00000000-0000-0000-0000-00000000cafe"}}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range testAuthHdr(seededProjectID, seededUserID) {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "database error") {
		t.Fatalf("status = %d, body = %s; want 500 database error from the instance read", w.Code, w.Body.String())
	}
}
