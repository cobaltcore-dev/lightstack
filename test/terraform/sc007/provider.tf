# SPEC-009 SC-007 / FR-018 — the UNMODIFIED Terraform OpenStack provider against lightstack.
# Auth (auth_url / user_name / password / tenant_name / region / domain) is read from
# the standard OS_* environment variables (source ~/.lightstack-env) — no edits needed.
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
