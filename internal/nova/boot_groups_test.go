package nova

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/neutron"
	"github.com/google/uuid"
)

// SPEC-033 T026 (FR-011, FR-012; research R1, R11): a boot gives its first port the groups the create
// resolved, default when none, and each later port the first port's current groups, so a change
// accepted between them reaches the later ports. The server's groups are stored on its ports only.

func bootTwoNetworks(t *testing.T, name, securityGroups string) (*fakeNeutron, string, string) {
	t.Helper()
	netA := seedBootNetwork(t, seededProjectID, "spec033-boot-a-"+uuid.NewString()[:6])
	netB := seedBootNetwork(t, seededProjectID, "spec033-boot-b-"+uuid.NewString()[:6])
	fake := &fakeNeutron{portFor: map[string]*neutron.PortInfo{
		netA: {ID: uuid.NewString(), NetworkID: netA, MAC: "fa:16:3e:33:00:0a"},
		netB: {ID: uuid.NewString(), NetworkID: netB, MAC: "fa:16:3e:33:00:0b"},
	}}
	_, r := newBootService(t, fake)
	cleanupServerNamed(t, name)
	body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":%q,"networks":[{"uuid":%q},{"uuid":%q}]%s}}`,
		name, seededImageID(t), netA, netB, securityGroups)
	if code, resp := doReq(r, http.MethodPost, "/v2.1/servers", body, bootHdr()); code != http.StatusAccepted {
		t.Fatalf("create: code=%d body=%s; want 202", code, resp)
	}
	if calls := awaitAllocations(t, fake, 2); len(calls) != 2 {
		t.Fatalf("allocations: %+v; want one per network", calls)
	}
	return fake, netA, netB
}

// awaitAllocations waits until the boot goroutine has made n allocations. What the boot does after
// them (on an unprivileged test host it cannot prepare the instance directory) does not matter here.
func awaitAllocations(t *testing.T, fake *fakeNeutron, n int) []allocCall {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if calls := fake.allocations(); len(calls) >= n {
			return calls
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fake.allocations()
}

func TestBootGroups_LaterPortsCopyTheFirst(t *testing.T) {
	requireDB(t)
	web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
	name := "spec033-boot-two"
	fake, netA, netB := bootTwoNetworks(t, name, fmt.Sprintf(`,"security_groups":[{"name":%q}]`, groupName(t, web)))

	calls := fake.allocations()
	if len(calls) != 2 {
		t.Fatalf("allocations: %+v; want two", calls)
	}
	if c := calls[0]; c.networkID != netA || !slices.Equal(c.groups, []string{web}) || c.copyFrom != "" {
		t.Errorf("first allocation: %+v; want network %s, groups [%s], no copy", c, netA, web)
	}
	if c := calls[1]; c.networkID != netB || len(c.groups) != 0 || c.copyFrom != fake.portFor[netA].ID {
		t.Errorf("second allocation: %+v; want network %s, no groups, copied from the first port %s", c, netB, fake.portFor[netA].ID)
	}

	// No stored copy: the ports are the only list of the server's groups (R1).
	var id string
	if err := testPool.QueryRow(t.Context(), "SELECT id FROM instances WHERE name = $1 AND project_id = $2",
		name, seededProjectID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if n := serverSecurityGroupCount(t, id); n != 0 {
		t.Errorf("server_security_groups rows: %d; want none", n)
	}
}

func TestBootGroups_NoneRequestedGivesDefault(t *testing.T) {
	requireDB(t)
	fake, _, _ := bootTwoNetworks(t, "spec033-boot-default", "")
	calls := fake.allocations()
	if len(calls) == 0 || !slices.Equal(calls[0].groups, []string{defaultGroup(t, seededProjectID)}) {
		t.Errorf("first allocation: %+v; want the project's default group", calls)
	}
}
