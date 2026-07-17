# SPEC-016 (cloud-storage#481) — Terraform OpenStack-provider compatibility smoke.
#
# Drives the UNMODIFIED terraform-provider-openstack against o3k, covering #481's
# four resource categories in a single apply/destroy:
#   - compute instances  (keypair + instance + volume-attach)
#   - networks           (network + subnet)
#   - security groups     (group + ingress rules, attached to the instance)
#   - volumes             (block volume + attach)
# Object storage is out of scope: o3k exposes no Swift API (see o3k#15).
#
# Pass = `terraform apply` and `terraform destroy` both succeed with no provider
# modification. Run against a live o3k in stub mode (see the 016 HOST_TESTING.md).

variable "image_name" {
  description = "Name of an existing Glance image (created in setup)."
  default     = "cirros"
}

variable "flavor_name" {
  description = "Name of an existing flavor (seeded by o3k)."
  default     = "m1.small"
}

# --- Image (Glance) — created in setup, resolved by name ---
data "openstack_images_image_v2" "img" {
  name = var.image_name
}

# --- Networks (Neutron): tenant network + subnet ---
resource "openstack_networking_network_v2" "net" {
  name = "tf-compat-net"
}

resource "openstack_networking_subnet_v2" "sub" {
  name       = "tf-compat-sub"
  network_id = openstack_networking_network_v2.net.id
  cidr       = "10.10.0.0/24"
  ip_version = 4
}

# --- Security groups (Neutron): group + ingress rules ---
resource "openstack_networking_secgroup_v2" "sg" {
  name        = "tf-compat-sg"
  description = "SPEC-016 Terraform provider-compat smoke"
}

resource "openstack_networking_secgroup_rule_v2" "ssh" {
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "tcp"
  port_range_min    = 22
  port_range_max    = 22
  remote_ip_prefix  = "0.0.0.0/0"
  security_group_id = openstack_networking_secgroup_v2.sg.id
}

resource "openstack_networking_secgroup_rule_v2" "icmp" {
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "icmp"
  remote_ip_prefix  = "0.0.0.0/0"
  security_group_id = openstack_networking_secgroup_v2.sg.id
}

# --- Compute (Nova): keypair + instance on the tenant network, with the SG ---
resource "openstack_compute_keypair_v2" "k" {
  name = "tf-compat-key"
}

resource "openstack_compute_instance_v2" "vm" {
  name            = "tf-compat-vm"
  image_id        = data.openstack_images_image_v2.img.id
  flavor_name     = var.flavor_name
  key_pair        = openstack_compute_keypair_v2.k.name
  security_groups = [openstack_networking_secgroup_v2.sg.name]

  network {
    uuid = openstack_networking_network_v2.net.id
  }

  depends_on = [openstack_networking_subnet_v2.sub]
}

# --- Volumes (Cinder): block volume + attach to the instance ---
resource "openstack_blockstorage_volume_v3" "vol" {
  name = "tf-compat-vol"
  size = 1
}

resource "openstack_compute_volume_attach_v2" "va" {
  instance_id = openstack_compute_instance_v2.vm.id
  volume_id   = openstack_blockstorage_volume_v3.vol.id
}

output "instance_id" { value = openstack_compute_instance_v2.vm.id }
output "network_id" { value = openstack_networking_network_v2.net.id }
output "subnet_id" { value = openstack_networking_subnet_v2.sub.id }
output "secgroup_id" { value = openstack_networking_secgroup_v2.sg.id }
output "volume_id" { value = openstack_blockstorage_volume_v3.vol.id }
