# SPEC-009 SC-007 / FR-018: the unmodified Terraform OpenStack provider drives a full
# single-node real-mode topology against O3K — instance + keypair + user_data + a
# floating IP + an attached volume — and tears it down cleanly.
#
# Run against a LIVE real-mode O3K (host; see quickstart.md §8):
#   cd test/terraform/sc007
#   terraform init
#   terraform apply -auto-approve
#   terraform destroy -auto-approve
# Pass = apply + destroy succeed with no provider modification, and the VM reflects
# the keypair, the user_data, and floating-IP reachability.

variable "image_name" { default = "cirros" }
variable "flavor_name" { default = "m1.small" }
variable "external_network_name" { default = "tf-sc007-ext" }

# Existing Glance image (created in the host setup).
data "openstack_images_image_v2" "img" {
  name = var.image_name
}

# --- Tenant network ---
resource "openstack_networking_network_v2" "net" {
  name = "tf-sc007-net"
}

resource "openstack_networking_subnet_v2" "sub" {
  name       = "tf-sc007-sub"
  network_id = openstack_networking_network_v2.net.id
  cidr       = "10.0.0.0/24"
  ip_version = 4
}

# --- External network + router (for the floating IP) ---
resource "openstack_networking_network_v2" "ext" {
  name     = var.external_network_name
  external = true
}

resource "openstack_networking_subnet_v2" "extsub" {
  name        = "tf-sc007-extsub"
  network_id  = openstack_networking_network_v2.ext.id
  cidr        = "172.24.4.0/24"
  ip_version  = 4
  enable_dhcp = false
}

resource "openstack_networking_router_v2" "r" {
  name                = "tf-sc007-router"
  external_network_id = openstack_networking_network_v2.ext.id
}

resource "openstack_networking_router_interface_v2" "ri" {
  router_id = openstack_networking_router_v2.r.id
  subnet_id = openstack_networking_subnet_v2.sub.id
}

# --- Keypair (Terraform-managed) ---
resource "openstack_compute_keypair_v2" "k" {
  name = "tf-sc007-key"
}

# --- Compute instance with user_data + the keypair ---
resource "openstack_compute_instance_v2" "vm" {
  name        = "tf-sc007-vm"
  image_id    = data.openstack_images_image_v2.img.id
  flavor_name = var.flavor_name
  key_pair    = openstack_compute_keypair_v2.k.name
  user_data   = "#!/bin/sh\ntouch /tmp/tf-sc007-userdata-ran\n"

  network {
    uuid = openstack_networking_network_v2.net.id
  }

  depends_on = [openstack_networking_subnet_v2.sub]
}

# --- Floating IP from the external pool, associated to the instance ---
resource "openstack_networking_floatingip_v2" "fip" {
  pool = openstack_networking_network_v2.ext.name
}

# Resolve the instance's port via the neutron API (ListPorts honors device_id).
# O3K's server detail doesn't populate the per-interface port id, so the usual
# openstack_compute_instance_v2.network[0].port is null — look it up explicitly.
data "openstack_networking_port_v2" "vm_port" {
  device_id  = openstack_compute_instance_v2.vm.id
  network_id = openstack_networking_network_v2.net.id
}

resource "openstack_networking_floatingip_associate_v2" "fip_assoc" {
  floating_ip = openstack_networking_floatingip_v2.fip.address
  port_id     = data.openstack_networking_port_v2.vm_port.id
}

# --- Block volume, attached to the instance ---
resource "openstack_blockstorage_volume_v3" "vol" {
  name = "tf-sc007-vol"
  size = 1
}

resource "openstack_compute_volume_attach_v2" "va" {
  instance_id = openstack_compute_instance_v2.vm.id
  volume_id   = openstack_blockstorage_volume_v3.vol.id
}

output "instance_id" { value = openstack_compute_instance_v2.vm.id }
output "fixed_ip" { value = openstack_compute_instance_v2.vm.access_ip_v4 }
output "floating_ip" { value = openstack_networking_floatingip_v2.fip.address }

# The provider-generated private key (no public_key was supplied) — used to SSH-verify
# reachability + key injection. Sensitive; only materialized in the gitignored state.
output "private_key" {
  value     = openstack_compute_keypair_v2.k.private_key
  sensitive = true
}
