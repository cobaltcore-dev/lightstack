package nova

import (
	"context"
	"errors"
	"fmt"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/rs/zerolog/log"
)

// Self-healing a server whose hypervisor guest has gone (SPEC-025 US4 / FR-025).
//
// Starting a server whose guest is absent used to be an unexplained 500, and the
// record went on claiming the server was running — so nothing surfaced the divergence
// until the next action also failed (cloud-storage#547 B12). The decision taken in
// clarify was to re-create the guest from the server's stored definition and start it,
// the same self-healing upstream's hard reboot performs.
//
// Nothing from the original create request is needed. Every input to a guest's
// definition is already persisted:
//
//	processors, memory, disk   the flavor row, via instances.flavor_id
//	network attachments        the instance's ports: device_id = <instance>, in its project
//	root disk                  the volume attached at the root device, else the qcow2 overlay
//	data volumes               the instance's other volume attachments
//	boot configuration         the instance's own config-drive ISO
//
// What the product did NOT have was a way to assemble that outside the creation path —
// the assembly lived inline in the VM-creation goroutine. Extracting it is what makes
// the self-heal possible, and is most of this file.

// buildVMSpecFromStored assembles a guest definition for an existing server using only
// what is recorded about it, so a domain can be re-created after libvirt has lost it.
//
// The returned spec is deliberately equivalent to the server as it was (FR-025a): same
// flavor-derived sizing, same root disk — its root volume when it booted from one — same
// network attachments, and the volumes attached to it since. A guest re-created with
// defaults instead would boot into something the user did not ask for, which is worse
// than failing.
func (svc *Service) buildVMSpecFromStored(ctx context.Context, instanceID string) (hypervisor.VMSpec, error) {
	var spec hypervisor.VMSpec

	var vcpus, ramMB, diskGB int
	if err := svc.pool.QueryRow(ctx, `
		SELECT f.vcpus, f.ram_mb, f.disk_gb
		FROM instances i
		JOIN flavors f ON f.id = i.flavor_id
		WHERE i.id = $1`, instanceID,
	).Scan(&vcpus, &ramMB, &diskGB); err != nil {
		return spec, fmt.Errorf("reading flavor for instance %s: %w", instanceID, err)
	}

	imagePath, err := svc.storedRootDisk(ctx, instanceID)
	if err != nil {
		return spec, err
	}

	networks, err := svc.storedNetworkConfigs(ctx, instanceID)
	if err != nil {
		return spec, err
	}

	volumes, err := svc.storedDataVolumes(ctx, instanceID)
	if err != nil {
		return spec, err
	}

	spec = hypervisor.VMSpec{
		UUID:      instanceID,
		Name:      fmt.Sprintf("instance-%s", instanceID[:8]),
		VCPUs:     vcpus,
		MemoryMB:  ramMB,
		DiskGB:    diskGB,
		ImagePath: imagePath,
		Networks:  networks,
		Volumes:   volumes,
	}

	// A non-nil CloudInit only tells the XML generator to attach the config-drive
	// cdrom; it reads the ISO path from the instance id. So the presence of the file
	// is the whole question — the meta/user data itself never needs rebuilding.
	if fileExists(hypervisor.CloudInitISOPath(instanceID)) {
		spec.CloudInit = &hypervisor.CloudInitConfig{}
	}

	return spec, nil
}

// storedRootDisk is the guest's root disk as recorded: the volume attached at the root device
// when the server booted from one — the boot path's own selection (volumeRootDiskPath) — and
// otherwise its overlay. Self-heal used to assume the overlay, so a server booted from a volume,
// which has none, was marked ERROR when its guest vanished though its root volume was intact.
//
// The overlay must already exist: re-creating it would discard the guest's data, so its absence
// is a refusal rather than something to paper over. An RBD root cannot be checked from the node,
// and the boot path does not check it either.
func (svc *Service) storedRootDisk(ctx context.Context, instanceID string) (string, error) {
	var volumeID, rbdPool, rbdImage string
	err := svc.pool.QueryRow(ctx, `
		SELECT v.id::text, COALESCE(v.rbd_pool, ''), COALESCE(v.rbd_image, '')
		  FROM volume_attachments a JOIN volumes v ON v.id = a.volume_id
		 WHERE a.instance_id = $1 AND a.device = $2`, instanceID, rootDevice,
	).Scan(&volumeID, &rbdPool, &rbdImage)
	if err == nil {
		return volumeRootDiskPath(volumeID, rbdPool, rbdImage), nil
	}
	if !errors.Is(err, database.ErrNoRows) {
		return "", fmt.Errorf("reading the root volume of instance %s: %w", instanceID, err)
	}
	overlay := hypervisor.OverlayPath(instanceID)
	if !fileExists(overlay) {
		return "", fmt.Errorf("instance %s has no disk at %s", instanceID, overlay)
	}
	return overlay, nil
}

// storedDataVolumes is every other volume attached to the server, at the device each was
// attached at, with the backing the attach path would give it (volumeDisk). Upstream's hard
// reboot defines the guest from all of its block devices; a guest re-created without them came
// back missing disks its record says it has.
func (svc *Service) storedDataVolumes(ctx context.Context, instanceID string) ([]hypervisor.DiskSpec, error) {
	rows, err := svc.pool.Query(ctx, `
		SELECT v.id::text, a.device, COALESCE(v.rbd_pool, ''), COALESCE(v.rbd_image, '')
		  FROM volume_attachments a JOIN volumes v ON v.id = a.volume_id
		 WHERE a.instance_id = $1 AND a.device <> $2
		 ORDER BY a.device`, instanceID, rootDevice)
	if err != nil {
		return nil, fmt.Errorf("reading volumes for instance %s: %w", instanceID, err)
	}
	defer rows.Close()

	var out []hypervisor.DiskSpec
	for rows.Next() {
		var volumeID, device, rbdPool, rbdImage string
		if err := rows.Scan(&volumeID, &device, &rbdPool, &rbdImage); err != nil {
			return nil, fmt.Errorf("scanning a volume for instance %s: %w", instanceID, err)
		}
		out = append(out, volumeDisk(volumeID, device, rbdPool, rbdImage))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating volumes for instance %s: %w", instanceID, err)
	}
	return out, nil
}

// storedNetworkConfigs reads the guest's interfaces from its ports — the same rows the
// server's address map is built from, read the same way: a port's device_id is whatever its
// creator wrote, so only the ports in the instance's own project are its interfaces. Another
// project's port naming this server would otherwise plug the revived guest into that project's
// network. Upstream lists them by tenant_id as well as device_id for the same reason.
func (svc *Service) storedNetworkConfigs(ctx context.Context, instanceID string) ([]hypervisor.NetworkConfig, error) {
	rows, err := svc.pool.Query(ctx,
		`SELECT p.id::text, p.mac_address, p.network_id::text
		   FROM ports p
		   JOIN instances i ON i.id = p.device_id AND i.project_id = p.project_id
		  WHERE i.id = $1
		  ORDER BY p.created_at`,
		instanceID)
	if err != nil {
		return nil, fmt.Errorf("reading ports for instance %s: %w", instanceID, err)
	}
	defer rows.Close()

	var out []hypervisor.NetworkConfig
	for rows.Next() {
		var portID, mac, networkID string
		if err := rows.Scan(&portID, &mac, &networkID); err != nil {
			return nil, fmt.Errorf("scanning port for instance %s: %w", instanceID, err)
		}
		out = append(out, hypervisor.NetworkConfig{
			PortID:     portID,
			MACAddress: mac,
			BridgeName: networking.BridgeNameForNetwork(networkID),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating ports for instance %s: %w", instanceID, err)
	}
	return out, nil
}

// reviveGuest re-creates and starts a server's hypervisor guest, returning the domain
// identifier the hypervisor assigned.
//
// One attempt, inside the caller's existing timeout — no retry and no backoff, per the
// fail-fast principle and FR-025e. If it cannot complete, the caller reports an
// action-impossible conflict rather than an internal fault (FR-025c, FR-026b): the
// server exists and the request was well formed, so this is a state the operator has
// to resolve, not an outage.
func (svc *Service) reviveGuest(ctx context.Context, instanceID string) (string, error) {
	spec, err := svc.buildVMSpecFromStored(ctx, instanceID)
	if err != nil {
		return "", fmt.Errorf("rebuilding the guest definition: %w", err)
	}

	domainID, err := svc.vmManager.CreateVM(ctx, hypervisor.GenerateVMXML(spec))
	if err != nil {
		return "", fmt.Errorf("re-creating the guest: %w", err)
	}

	// Deliberately loud. A guest that keeps vanishing is a real fault, and a start
	// that quietly fixes it every time would turn that into a cost nobody notices
	// (FR-025b). The tenant-visible half is the action record, written by the caller.
	log.Warn().
		Str("instance_id", instanceID).
		Str("domain_id", domainID).
		Int("vcpus", spec.VCPUs).
		Int("memory_mb", spec.MemoryMB).
		Int("interfaces", len(spec.Networks)).
		Msg("self-heal: the server's guest was absent and has been re-created from stored state")

	return domainID, nil
}
