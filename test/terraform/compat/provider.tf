# SPEC-016 (cloud-storage#481) — the UNMODIFIED Terraform OpenStack provider
# against Lightstack. Auth (auth_url / user_name / password / tenant_name / region /
# domain) is read from the standard OS_* environment variables — no edits needed.
terraform {
  required_version = ">= 1.5"
  required_providers {
    openstack = {
      source  = "terraform-provider-openstack/openstack"
      version = "~> 3.0"
    }
  }
}

provider "openstack" {}
