# Lightstack Troubleshooting Guide

## TAP Device and Bridge Networking Issues

### Issue: routers or DHCP gone after a restart of Lightstack

**Symptom**: after the container or pod restarts, `ip netns list` on the network node shows no
`qrouter-*` or `qdhcp-*`, and north-south traffic stops until the reconciler rebuilds them.

**Root Cause**: the namespaces were made inside the container's own `/run/netns`, which went with it.
Lightstack's namespaces must be the host's.

**Solution**: run with the host's `/run/netns` mounted shared both ways. The KVM Compose file mounts
`/run/netns:/run/netns:rshared`, and the chart mounts it with `mountPropagation: Bidirectional`; nothing
needs creating by hand, except under Podman, which does not create a missing bind source:
`sudo mkdir -p /run/netns` once. The host's `/run` must be a shared mount, as systemd makes it
(`findmnt -o TARGET,PROPAGATION /run`).

### Issue: readiness fails with `bridge-netfilter unavailable`

**Symptom**: `/readyz` answers `503 bridge-netfilter unavailable`; the log says which setting does not
hold.

**Root Cause**: the ports' chains see bridged traffic only with the `br_netfilter` module loaded and
`net.bridge.bridge-nf-call-iptables` and `-ip6tables` at `1`. Lightstack loads and sets them at start;
without `/lib/modules` mounted, or on a kernel without the module, it cannot.

**Solution**: mount `/lib/modules` read-only (the chart and the KVM Compose file do), or load the module
on the host: `sudo modprobe br_netfilter`. Readiness follows the settings, so it recovers without a
restart once they hold.

### Issue: "Cannot get interface MTU on 'br-xxx': No such device"

**Symptom**: VM creation fails with libvirt error:
```
Cannot get interface MTU on 'br-3426f435': No such device
```

**Root Cause**: Bridges were created inside project network namespaces, but libvirt runs on the host and cannot access namespaced bridges.

**Solution**: bridges are created in the namespace Lightstack itself runs in, for libvirt access. This is
the correct architecture:
- Bridges, the guests' TAPs (libvirt's, `tap<port id[:11]>`) and VXLAN devices: all in the namespace
  Lightstack runs in; no bridge holds an address
- VMs: connect via those bridges
- Per-network layer-2 isolation: one bridge per network — not a namespace per project
- Routers and each network's DHCP live in `qrouter-*` and `qdhcp-*` namespaces on the network node
- Nothing creates `light-ns-<project>` any more; teardown only reclaims what older deployments left

`BridgeManager` and `VXLANManager.AttachToBridge` take **no namespace argument** — the in-namespace
variants were removed once their last callers went away, so these devices can only be placed beside
each other.

### Issue: "Failed to get write lock: Is another process using the image"

**Symptom**: QEMU fails to start VM:
```
Failed to get "write" lock
Is another process using the image [/var/lib/lightstack/images/xxx.qcow2]?
```

**Root Cause**: Previous VM instances are still running and using the image file.

**Solution**:
1. List all VMs:
   ```bash
   sudo virsh list --all
   ```

2. Destroy and undefine old VMs:
   ```bash
   sudo virsh destroy instance-xxx
   sudo virsh undefine instance-xxx
   ```

3. Verify no VMs are using the image:
   ```bash
   sudo lsof /var/lib/lightstack/images/*.qcow2
   ```

## Silent VM Creation Failures

### Issue: Instances go to ERROR status with no logs

**Symptom**: VMs reach ERROR status immediately with no error messages in logs.

**Root Causes**:
1. Panics in goroutines not caught
2. Insufficient debug logging
3. Image file path mismatches (UUID.qcow2 vs actual filename)
4. Image status not "active" in database

**Solutions**:

1. Add panic recovery in handlers.go:
   ```go
   defer func() {
       if r := recover(); r != nil {
           log.Printf("PANIC in VM creation: %v", r)
           svc.pool.Exec(ctx, "UPDATE instances SET status = 'ERROR' WHERE id = $1", instanceID)
       }
   }()
   ```

2. Add extensive debug logging:
   ```go
   log.Printf("DEBUG: VM creation goroutine started for instance %s", instanceID)
   log.Printf("DEBUG: Allocating ports for instance %s", instanceID)
   // ... more logging at each step
   ```

3. Fix image file paths:
   ```bash
   # Create symlink if needed
   cd /var/lib/lightstack/images
   ln -sf actual-image.img UUID.qcow2
   ```

4. Fix image status:
   ```sql
   UPDATE images SET status='active', size_bytes=21000000 WHERE name='cirros-0.6.2';
   ```

## Verification Commands

### Check VM Status
```bash
# Via OpenStack API
TOKEN=$(curl -s -X POST http://localhost:35357/v3/auth/tokens ...)
curl -H "X-Auth-Token: $TOKEN" http://localhost:8774/v2.1/servers/detail | jq '.servers'

# Via virsh
sudo virsh list --all
```

### Check Networking
```bash
# Check bridges
sudo brctl show

# Check the guests' TAP devices (libvirt's, tap<port id[:11]>)
sudo ip link show | grep tap

# Check interfaces in bridge
sudo brctl show br-3426f435

# Check the routers' and networks' DHCP namespaces (on the network node)
sudo ip netns list
sudo ip netns exec qrouter-<router id> ip addr show
sudo ip netns exec qdhcp-<network id> ip addr show

# Check a port's chains (on the node its guest runs on)
sudo iptables -S | grep -- 'ls-[ios]'
```

### Check Lightstack Logs
```bash
# Docker deployment
docker logs lightstack --tail 100

# Look for specific errors
docker logs lightstack 2>&1 | grep -E 'ERROR|failed'

# Check debug logs
docker logs lightstack 2>&1 | grep DEBUG
```

## Common Diagnostic Steps

1. **Verify host requirements**:
   ```bash
   # Check KVM support
   lsmod | grep kvm
   
   # Check libvirt
   sudo systemctl status libvirtd
   
   # Check networking
   sudo ip link show
   ```

2. **Verify Lightstack configuration**:
   ```bash
   # Check mode settings
   grep -E 'libvirt_mode|networking_mode' /opt/lightstack/config/lightstack.yaml
   
   # Verify database connection
   psql -U lightstack -d lightstack -c "SELECT COUNT(*) FROM instances;"
   ```

3. **Check file permissions**:
   ```bash
   # libvirt socket: virtqemud-sock where the modular daemons run, else libvirt-sock
   ls -la /var/run/libvirt/virtqemud-sock /var/run/libvirt/libvirt-sock
   
   # Image directory
   ls -la /var/lib/lightstack/images/
   
   # the host's namespaces, as Lightstack sees them
   ls -la /run/netns/
   ```

4. **Review recent changes**:
   ```bash
   # Check lightstack container status
   docker ps | grep lightstack
   
   # Check container restarts
   docker inspect lightstack | jq '.[].State'
   
   # Check mounted volumes
   docker inspect lightstack | jq '.[].Mounts'
   ```
