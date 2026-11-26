# Lightstack Deployment Scripts

Automated deployment scripts for Lightstack installation.

---

## deploy-single-node.sh

**Purpose**: Interactive single-node Lightstack deployment with KVM hypervisor

**What it does**:
- ✅ Checks hardware requirements (CPU, RAM, disk, virtualization support)
- ✅ Installs KVM/libvirt hypervisor
- ✅ Configures network bridge for external connectivity
- ✅ Installs PostgreSQL 18 database
- ✅ Installs Docker and Docker Compose
- ✅ Clones and builds Lightstack from source
- ✅ Creates optimized configuration file
- ✅ Runs database migrations
- ✅ Creates systemd service for Lightstack
- ✅ Optionally deploys Horizon dashboard + noVNC
- ✅ Configures firewall (UFW)
- ✅ Installs OpenStack CLI tools
- ✅ Verifies installation
- ✅ Generates environment file for easy CLI access

**Requirements**:
- Fresh Ubuntu 24.04/22.04 or Debian 12 installation
- Root/sudo access
- Minimum: 4 CPU cores, 16GB RAM, 100GB disk
- CPU with VT-x/AMD-V virtualization support
- Internet connection

**Usage**:

```bash
# Download the script
wget https://raw.githubusercontent.com/cobaltcore-dev/lightstack/main/scripts/deploy-single-node.sh

# Make executable
chmod +x deploy-single-node.sh

# Run as root
sudo ./deploy-single-node.sh
```

**Interactive Prompts**:

The script will ask you to configure:

1. **Hostname** - Name for this node (default: lightstack-demo)
2. **Host IP** - IP address for this host (auto-detected)
3. **Network Interface** - Primary network interface (auto-detected)
4. **Gateway** - Network gateway (auto-detected)
5. **DNS Servers** - DNS server addresses (default: 8.8.8.8,8.8.4.4)
6. **PostgreSQL Password** - Database password (auto-generated if not provided)
7. **Storage Paths** - Paths for volumes, images, instances (defaults provided)
8. **Horizon Deployment** - Deploy Horizon dashboard? (default: Yes)

**Example Session**:

```bash
$ sudo ./deploy-single-node.sh

╔═══════════════════════════════════════════════════════════╗
║                                                           ║
║   lightstack Single-Node Deployment Script                      ║
║   Deploy lightstack + KVM Hypervisor on Single Host            ║
║                                                           ║
╚═══════════════════════════════════════════════════════════╝

[INFO] Detected OS: ubuntu 24.04
[INFO] Checking hardware requirements...
[SUCCESS] CPU cores: 8 ✓
[SUCCESS] RAM: 32GB ✓
[SUCCESS] Disk space: 250GB ✓
[SUCCESS] Virtualization support: enabled ✓

[INFO] Starting interactive configuration...

Enter hostname for this node [lightstack-demo]: my-cloud
Enter host IP address [192.168.1.100]:
Enter primary network interface [eth0]:
Enter network gateway [192.168.1.1]:
Enter DNS servers [8.8.8.8,8.8.4.4]:
Enter PostgreSQL password for lightstack database: ********
Enter node-local state root [/var/lib/lightstack]:
Deploy Horizon dashboard? [Y/n]: Y

[INFO] Configuration Summary:
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  Hostname:           my-cloud
  Host IP:            192.168.1.100
  Network Interface:  eth0
  Gateway:            192.168.1.1
  DNS Servers:        8.8.8.8,8.8.4.4
  State Root:         /var/lib/lightstack
  Deploy Horizon:     Y
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

Proceed with installation? [Y/n]: Y

[INFO] Starting installation...
[INFO] Updating system packages...
[SUCCESS] System updated
[INFO] Configuring hostname: my-cloud
[SUCCESS] Hostname configured
...
```

**Installation Time**:
- Typical installation: 15-20 minutes
- Depends on: internet speed, hardware performance

**What Gets Installed**:

```
/opt/lightstack/                          # lightstack installation
├── bin/lightstack                        # lightstack binary
├── config/lightstack.yaml                # Configuration file
└── ...

/opt/horizon/                      # Horizon dashboard (optional)
├── docker-compose.yml
└── local_settings.py

/var/lib/lightstack/                      # Storage directories
├── volumes/                       # Cinder volumes
├── images/                        # Glance images
└── instances/                     # Nova instances

/etc/systemd/system/lightstack.service    # Systemd service

/root/.lightstack-env                     # Environment file for CLI
```

**Post-Installation**:

After successful installation:

```bash
# Source environment
source /root/.lightstack-env

# Test authentication
openstack token issue

# List services
openstack service list

# Access Horizon (if deployed)
open http://YOUR_HOST_IP

# Login credentials:
#   Domain: Default
#   Username: admin
#   Password: secret
```

**Verify Installation**:

```bash
# Check lightstack service status
systemctl status lightstack.service

# View lightstack logs
journalctl -u lightstack.service -f

# Check Horizon (if deployed)
docker ps | grep horizon

# Test OpenStack CLI
openstack --version
openstack catalog list
```

**Troubleshooting**:

1. **KVM acceleration not available**:
   - Check BIOS/UEFI settings for VT-x/AMD-V
   - Run: `kvm-ok` to verify

2. **Network bridge fails**:
   - Check network interface name: `ip link show`
   - Verify netplan syntax: `netplan try`
   - Rollback: `cp /etc/netplan/backup/*.yaml /etc/netplan/`

3. **lightstack service won't start**:
   - Check logs: `journalctl -u lightstack.service -n 100`
   - Verify PostgreSQL: `systemctl status postgresql`
   - Check libvirt: `systemctl status libvirtd`

4. **Horizon not accessible**:
   - Check container: `docker ps`
   - View logs: `cd /opt/horizon && docker compose logs horizon`
   - Restart: `docker compose restart`

**Uninstall**:

To remove lightstack:

```bash
# Stop services
systemctl stop lightstack.service
systemctl disable lightstack.service
cd /opt/horizon && docker compose down

# Remove installation
rm -rf /opt/lightstack /opt/horizon
rm /etc/systemd/system/lightstack.service
rm /root/.lightstack-env

# Remove storage (CAUTION: deletes all VMs, volumes, images)
rm -rf /var/lib/lightstack

# Restore network (optional)
cp /etc/netplan/backup/*.yaml /etc/netplan/
rm /etc/netplan/01-lightstack-bridge.yaml
netplan apply
```

**Related Documentation**:
- [SINGLE_NODE_DEPLOYMENT.md](../docs/SINGLE_NODE_DEPLOYMENT.md) - Manual deployment guide
- [SCALING.md](../docs/SCALING.md) - Production multi-node deployment
- [CONFIGURATION.md](../docs/CONFIGURATION.md) - Configuration reference

---

## Future Scripts

Planned deployment scripts:

- `deploy-multi-node.sh` - Multi-node cluster deployment
- `deploy-ha-cluster.sh` - High availability cluster setup
- `add-compute-node.sh` - Add compute node to existing cluster
- `backup-lightstack.sh` - Backup configuration and database
- `restore-lightstack.sh` - Restore from backup

---

**Last Updated**: March 17, 2026
