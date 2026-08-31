package nova

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/cobaltcore-dev/lightstack/pkg/storage"
	"github.com/google/uuid"
)

// Self-heal and volumes (PR #91 review). The rebuild assumed every server boots from its own
// overlay, so a server booted from a volume — which has none — was marked ERROR when its guest
// vanished, though its root volume was intact. And it re-created the guest without the volumes
// attached to it later, so a revived server came back missing disks its record says it has.
// Upstream's hard reboot, which this mirrors, defines the guest from all of its block devices.

// attachSeededVolume records a volume attached to instanceID at device, as the attach and the
// boot-from-volume paths record one.
func attachSeededVolume(t *testing.T, instanceID, device, rbdImage string) string {
	t.Helper()
	ctx := context.Background()
	volumeID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO volumes (id, name, project_id, size_gb, status, bootable, rbd_pool, rbd_image, attached_to_instance_id)
		 VALUES ($1, $2, $3, 1, 'in-use', true, 'volumes', $4, $5)`,
		volumeID, "spec025-selfheal-"+strings.TrimPrefix(device, "/dev/"), seededProjectID, rbdImage, instanceID); err != nil {
		t.Fatalf("seeding volume: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO volume_attachments (id, volume_id, instance_id, device, attached_at) VALUES ($1, $2, $3, $4, now())`,
		uuid.NewString(), volumeID, instanceID, device); err != nil {
		t.Fatalf("seeding attachment: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM volume_attachments WHERE volume_id = $1", volumeID)
		_, _ = testPool.Exec(context.Background(), "DELETE FROM volumes WHERE id = $1", volumeID)
	})
	return volumeID
}

// withLocalVolumes makes the given volumes' node-local backing files appear present.
func withLocalVolumes(t *testing.T, volumeIDs ...string) {
	t.Helper()
	present := map[string]bool{}
	for _, id := range volumeIDs {
		present[storage.LocalVolumePath(id)] = true
	}
	prev := fileExists
	fileExists = func(path string) bool { return present[path] || prev(path) }
	t.Cleanup(func() { fileExists = prev })
}

func TestRebuildBootsAVolumeBackedServerFromItsRootVolume(t *testing.T) {
	requireDB(t)
	svc, _, mgr := newLifecycleService(t)

	// No overlay is staged for either server: a volume-backed server has none.
	local, _ := seedLifecycleInstance(t, mgr, "spec025-selfheal-root-local")
	localRoot := attachSeededVolume(t, local, "/dev/vda", "volume-root-local")
	withLocalVolumes(t, localRoot)
	spec, err := svc.buildVMSpecFromStored(context.Background(), local)
	if err != nil {
		t.Fatalf("rebuilding a server booted from a local volume: %v", err)
	}
	if want := storage.LocalVolumePath(localRoot); spec.ImagePath != want {
		t.Errorf("root disk = %q; want the root volume's own file %q", spec.ImagePath, want)
	}

	rbd, _ := seedLifecycleInstance(t, mgr, "spec025-selfheal-root-rbd")
	attachSeededVolume(t, rbd, "/dev/vda", "volume-root-rbd")
	spec, err = svc.buildVMSpecFromStored(context.Background(), rbd)
	if err != nil {
		t.Fatalf("rebuilding a server booted from an RBD volume: %v", err)
	}
	if want := "rbd:volumes/volume-root-rbd"; spec.ImagePath != want {
		t.Errorf("root disk = %q; want the root volume's RBD image %q", spec.ImagePath, want)
	}
	if len(spec.Volumes) != 0 {
		t.Errorf("the root volume also appears as a data volume: %+v", spec.Volumes)
	}
}

func TestRebuildReattachesTheServersDataVolumes(t *testing.T) {
	requireDB(t)
	svc, _, mgr := newLifecycleService(t)
	id, _ := seedLifecycleInstance(t, mgr, "spec025-selfheal-data")
	withInstanceDisk(t, id) // booted from an image: the overlay is its root

	localData := attachSeededVolume(t, id, "/dev/vdb", "volume-data-local")
	attachSeededVolume(t, id, "/dev/vdc", "volume-data-rbd")
	withLocalVolumes(t, localData)
	other, _ := seedLifecycleInstance(t, mgr, "spec025-selfheal-data-other")
	attachSeededVolume(t, other, "/dev/vdb", "volume-someone-elses") // must not appear

	spec, err := svc.buildVMSpecFromStored(context.Background(), id)
	if err != nil {
		t.Fatalf("rebuilding: %v", err)
	}
	if spec.ImagePath != hypervisor.OverlayPath(id) {
		t.Errorf("root disk = %q; want the overlay %q", spec.ImagePath, hypervisor.OverlayPath(id))
	}
	want := []hypervisor.DiskSpec{
		{Device: "/dev/vdb", Type: "file", Source: storage.LocalVolumePath(localData)},
		{Device: "/dev/vdc", Type: "network", Source: "volumes/volume-data-rbd", Protocol: "rbd"},
	}
	if len(spec.Volumes) != len(want) {
		t.Fatalf("rebuilt guest has %d data volumes (%+v); want the %d it had", len(spec.Volumes), spec.Volumes, len(want))
	}
	for i := range want {
		if spec.Volumes[i] != want[i] {
			t.Errorf("data volume %d = %+v; want %+v", i, spec.Volumes[i], want[i])
		}
	}
	xml := hypervisor.GenerateVMXML(spec)
	for _, element := range []string{"<target dev='vdb' bus='virtio'/>", "<target dev='vdc' bus='virtio'/>",
		"<source file='" + storage.LocalVolumePath(localData) + "'/>"} {
		if !strings.Contains(xml, element) {
			t.Errorf("the re-created guest's definition lacks %s", element)
		}
	}
}

func TestStartSelfHealsAVolumeBackedServer(t *testing.T) {
	requireDB(t)
	_, r, mgr := newLifecycleService(t)
	id, domainID := seedLifecycleInstance(t, mgr, "spec025-selfheal-volume-backed")
	withLocalVolumes(t, attachSeededVolume(t, id, "/dev/vda", "volume-root"))

	if err := mgr.DeleteVM(t.Context(), domainID); err != nil {
		t.Fatalf("removing the stub guest: %v", err)
	}
	if _, err := testPool.Exec(t.Context(),
		`UPDATE instances SET status = 'SHUTOFF', power_state = 4 WHERE id = $1`, id); err != nil {
		t.Fatalf("pre-setting the stopped state: %v", err)
	}
	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"os-start": null}`, lifecycleHdr())
	if code != http.StatusAccepted {
		t.Fatalf("os-start of a volume-backed server whose guest vanished = %d; want 202. body: %s", code, body)
	}
	if status, power := readInstanceState(t, id); status != "ACTIVE" || power != 1 {
		t.Errorf("after the self-heal: %s/%d; want ACTIVE/1, not ERROR", status, power)
	}
}
