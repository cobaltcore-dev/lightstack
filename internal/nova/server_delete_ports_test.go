package nova

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T138 (FR-008d, FR-008e, FR-040; research R25, R34): a server's deletion withdraws
// its ports from their units after its domain is gone and before their records go. A withdrawal that
// fails answers 500 and keeps the server and its ports; a second deletion, the domain gone already,
// finishes. DELETE and forceDelete alike.

func TestServerDeleteWithdrawsItsPorts(t *testing.T) {
	requireDB(t)
	for _, verb := range []string{"delete", "forceDelete"} {
		t.Run(verb, func(t *testing.T) {
			ctx := context.Background()
			fake := &fakeNeutron{withdrawErr: errors.New("the node could not converge the ports")}
			svc, r := newBootService(t, fake)
			instance, domain := seedLifecycleInstance(t, svc.vmManager, "spec033-del-"+uuid.NewString()[:6])
			network := seedBootNetwork(t, seededProjectID, "spec033-del-net")
			var ports []string
			for range 2 {
				id := uuid.NewString()
				if _, err := testPool.Exec(ctx, `INSERT INTO ports (id, network_id, project_id, device_id, device_owner, mac_address, status, fixed_ips)
					VALUES ($1, $2, $3, $4, 'compute:nova', 'fa:16:3e:00:85:01', 'ACTIVE', '[]'::jsonb)`, id, network, seededProjectID, instance); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM ports WHERE id = $1", id) })
				ports = append(ports, id)
			}
			slices.Sort(ports)
			domainGone := false
			fake.onWithdraw = func([]string) {
				_, _, err := svc.vmManager.GetVMState(ctx, domain)
				domainGone = errors.Is(err, hypervisor.ErrDomainNotFound)
			}
			remove := func(t *testing.T, want int) {
				t.Helper()
				method, path, body := http.MethodDelete, "/v2.1/servers/"+instance, ""
				if verb == "forceDelete" {
					method, path, body = http.MethodPost, "/v2.1/servers/"+instance+"/action", `{"forceDelete":null}`
				}
				code, resp := doReq(r, method, path, body, bootHdr())
				if code != want {
					t.Fatalf("%s: code=%d body=%s; want %d", verb, code, resp, want)
				}
			}
			count := func(query string) int {
				var n int
				if err := testPool.QueryRow(ctx, query, instance).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}

			remove(t, http.StatusInternalServerError)
			if count("SELECT count(*) FROM instances WHERE id = $1") != 1 || count("SELECT count(*) FROM ports WHERE device_id = $1") != 2 {
				t.Fatal("a failed withdrawal deleted the server or its ports; want both kept")
			}
			if len(fake.withdrawn) != 1 || !slices.Equal(sortedIDs(fake.withdrawn[0]), ports) || !domainGone {
				t.Errorf("withdrawals %v (domain gone %v); want one of both ports, after the domain", fake.withdrawn, domainGone)
			}

			fake.withdrawErr = nil
			remove(t, http.StatusNoContent)
			if count("SELECT count(*) FROM instances WHERE id = $1") != 0 || count("SELECT count(*) FROM ports WHERE device_id = $1") != 0 {
				t.Error("the second deletion left the server or its ports")
			}
			if fake.restored != 0 {
				t.Errorf("%d withdrawal(s) restored after a deletion that stood; want none", fake.restored)
			}
		})
	}
}

func sortedIDs(ids []string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}
