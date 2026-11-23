package neutron

import (
	"context"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/compute"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T144 (FR-040's "state on nodes other than the network node ... by their
// reconciliation", FR-044; research R26, R34): the VXLAN coordinator keeps a node's devices and
// forwarding entries matching the records both ways. An entry Lightstack wrote whose row is gone — its
// port deleted, which takes the row with it — is removed, and a deleted network's device too; neither
// after an incomplete read.

// fakeVXLAN is a node's VXLAN devices, by name, and the entries Lightstack wrote on them, per network.
type fakeVXLAN struct {
	mu      sync.Mutex
	devices map[string]bool
	entries map[string]map[string]string // network → MAC → remote tunnel address
}

func newFakeVXLAN() *fakeVXLAN {
	return &fakeVXLAN{devices: map[string]bool{}, entries: map[string]map[string]string{}}
}

func (f *fakeVXLAN) GetVXLANName(networkID string) string { return "vxlan-" + networkID[:8] }

func (f *fakeVXLAN) CreateVXLAN(networkID string, _ int, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devices[f.GetVXLANName(networkID)] = true
	if f.entries[networkID] == nil {
		f.entries[networkID] = map[string]string{}
	}
	return nil
}

func (f *fakeVXLAN) AttachToBridge(string, string) error { return nil }

func (f *fakeVXLAN) AddFDBEntry(networkID, mac, remote string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries[networkID] == nil {
		f.entries[networkID] = map[string]string{}
	}
	f.entries[networkID][mac] = remote
	return nil
}

func (f *fakeVXLAN) RemoveFDBEntry(networkID, mac string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries[networkID], mac)
	return nil
}

func (f *fakeVXLAN) ListFDBEntries(networkID string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.entries[networkID]), nil
}

func (f *fakeVXLAN) VXLANDevices() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.devices)), nil
}

func (f *fakeVXLAN) DeleteVXLANDevice(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.devices, name)
	for network := range f.entries {
		if "vxlan-"+network[:8] == name {
			delete(f.entries, network)
		}
	}
	return nil
}

func (f *fakeVXLAN) holds(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.devices[name]
}

func TestVXLANCoordinatorRemovesWhatNoRecordHolds(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	const local, remote, gone = "192.0.2.211", "192.0.2.212", "192.0.2.213"
	registry, err := compute.NewNodeRegistry(testPool, "", local, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	seedComputeNode(t, "spec033-vxlan-"+uuid.NewString()[:6], remote, "active", time.Now())

	network := seedNetwork(t, "spec033-vxlan-"+uuid.NewString()[:6])
	mustExec(t, "UPDATE networks SET network_type = 'vxlan' WHERE id = $1", network)
	mustExec(t, "INSERT INTO network_vni_allocations (network_id, vni) VALUES ($1, 16777190)", network)
	port := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
		VALUES ($1, 'spec033-vxlan', $2, $3, 'fa:16:3e:44:00:01', 'ACTIVE', '[]')`, port, network, seededProjectID)
	mustExec(t, `INSERT INTO vxlan_fdb_entries (network_id, mac_address, vtep_ip, port_id)
		VALUES ($1, 'fa:16:3e:44:00:01', $2, $3)`, network, remote, port)
	// A port on a node that is no longer active: its row stands, its entry goes.
	stranded := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
		VALUES ($1, 'spec033-vxlan', $2, $3, 'fa:16:3e:44:00:03', 'ACTIVE', '[]')`, stranded, network, seededProjectID)
	mustExec(t, `INSERT INTO vxlan_fdb_entries (network_id, mac_address, vtep_ip, port_id)
		VALUES ($1, 'fa:16:3e:44:00:03', $2, $3)`, network, gone, stranded)

	fake := newFakeVXLAN()
	vc := NewVXLANCoordinator(testPool, fake, registry, 0, VNIRange{Start: 16777190, End: 16777199})
	// The pass reads only this test's network and rows, as if the database held nothing else.
	readNetworks, readRows := vc.readNetworks, vc.readRows
	vc.readNetworks = func(ctx context.Context) ([]vxlanNetwork, bool, error) {
		all, complete, err := readNetworks(ctx)
		return slices.DeleteFunc(all, func(n vxlanNetwork) bool { return n.id != network }), complete, err
	}
	vc.readRows = func(ctx context.Context) ([]fdbRow, bool, error) {
		all, complete, err := readRows(ctx)
		return slices.DeleteFunc(all, func(r fdbRow) bool { return r.network != network }), complete, err
	}

	// Before the pass: an entry whose port, and with it its row, was deleted; one of the port on the node
	// that is no longer active; and the device of a network deleted meanwhile.
	deleted := uuid.NewString()
	fake.devices["vxlan-"+deleted[:8]] = true
	fake.entries[network] = map[string]string{"fa:16:3e:44:00:02": remote, "fa:16:3e:44:00:03": gone}

	vc.sync(ctx)

	if got, _ := fake.ListFDBEntries(network); !maps.Equal(got, map[string]string{"fa:16:3e:44:00:01": remote}) {
		t.Errorf("entries after the pass: %v; want the recorded port's alone", got)
	}
	if !fake.holds(fake.GetVXLANName(network)) {
		t.Error("the network's device is missing")
	}
	if fake.holds("vxlan-" + deleted[:8]) {
		t.Error("a deleted network's device was kept")
	}

	t.Run("a partial read removes nothing", func(t *testing.T) {
		fake.devices["vxlan-"+deleted[:8]] = true
		_ = fake.AddFDBEntry(network, "fa:16:3e:44:00:02", remote)
		rows, networks := vc.readRows, vc.readNetworks
		defer func() { vc.readRows, vc.readNetworks = rows, networks }()
		vc.readRows = func(ctx context.Context) ([]fdbRow, bool, error) {
			r, _, err := rows(ctx)
			return r, false, err
		}
		vc.readNetworks = func(ctx context.Context) ([]vxlanNetwork, bool, error) {
			n, _, err := networks(ctx)
			return n, false, err
		}
		vc.sync(ctx)
		if got, _ := fake.ListFDBEntries(network); got["fa:16:3e:44:00:02"] != remote {
			t.Errorf("entries after a partial read: %v; want the unrecorded one kept", got)
		}
		if !fake.holds("vxlan-" + deleted[:8]) {
			t.Error("a device was removed after a partial read")
		}
	})

	t.Run("a failed read removes nothing", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		vc.sync(cctx)
		if got, _ := fake.ListFDBEntries(network); got["fa:16:3e:44:00:02"] != remote {
			t.Errorf("entries after a failed read: %v; want the unrecorded one kept", got)
		}
		if !fake.holds("vxlan-" + deleted[:8]) {
			t.Error("a device was removed after a failed read")
		}
	})
}
