package neutron

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T263, T264 (FR-008, FR-041): a port update whose firewall change cannot be applied undoes all of
// it, compare-and-set, its other fields included: they were stored beside the groups and stayed after the
// 500. And a device_owner change that moves the port into or out of network:* — which has no chains, its leg
// trusted by name — converges the port's node before the answer, as a groups change does.
func TestPortUpdateAppliesOrUndoesAllOfItself(t *testing.T) {
	requireDB(t)
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	t.Run("a failed groups change takes its other fields back with it", func(t *testing.T) {
		r, svc, fw := newPortAPIRig(t)
		port := sgTestPort(t)
		old := sgTestGroup(t, seededProjectID, "spec033-t263-old-"+uuid.NewString()[:6])
		sgTestAttach(t, port, old)
		web := sgTestGroup(t, seededProjectID, "spec033-t263-web-"+uuid.NewString()[:6])
		bind(t, port, svc.nodes.Name)
		before := sgTestPortName(t, port)
		fw.failFirst(1, errors.New("iptables-restore failed"))
		if code, body := putPort(t, r, projectHdr(seededProjectID), port,
			`{"port":{"name":"spec033-t263-renamed","security_groups":`+quoted(web)+`}}`); code != http.StatusInternalServerError {
			t.Fatalf("code=%d body=%s; want 500", code, body)
		}
		if got := sgTestGroupsOf(t, port); len(got) != 1 || got[0] != old {
			t.Errorf("groups %v after the failed update; want [%s] back", got, old)
		}
		if got := sgTestPortName(t, port); got != before {
			t.Errorf("name %q after the failed update; want %q back", got, before)
		}
	})

	t.Run("a device_owner change into network:* converges the port's node", func(t *testing.T) {
		r, svc, fw := newPortAPIRig(t)
		port := sgTestPort(t)
		web := sgTestGroup(t, seededProjectID, "spec033-t264-"+uuid.NewString()[:6])
		sgTestAttach(t, port, web)
		bind(t, port, svc.nodes.Name)
		if code, body := putPort(t, r, admin, port, sgBody(quoted(web))); code != http.StatusOK {
			t.Fatalf("groups: code=%d body=%s", code, body)
		}
		if _, held := fw.port(port); !held {
			t.Fatal("the node holds no chains for the bound port")
		}
		if code, body := putPort(t, r, admin, port, `{"port":{"device_owner":"network:dhcp"}}`); code != http.StatusOK {
			t.Fatalf("device_owner: code=%d body=%s", code, body)
		}
		if _, held := fw.port(port); held {
			t.Error("the node still holds chains for a port that became network:dhcp")
		}
	})

	t.Run("a failed device_owner change is undone", func(t *testing.T) {
		r, svc, fw := newPortAPIRig(t)
		port := sgTestPort(t)
		bind(t, port, svc.nodes.Name)
		fw.failFirst(1, errors.New("iptables-restore failed"))
		if code, body := putPort(t, r, admin, port, `{"port":{"device_owner":"network:dhcp"}}`); code != http.StatusInternalServerError {
			t.Fatalf("code=%d body=%s; want 500", code, body)
		}
		var owner *string
		if err := testPool.QueryRow(context.Background(), "SELECT device_owner FROM ports WHERE id = $1", port).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if owner != nil && *owner == "network:dhcp" {
			t.Error("device_owner network:dhcp kept after the failed update")
		}
	})
}
