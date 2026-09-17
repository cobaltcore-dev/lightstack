# Host testing guide — SPEC-017 Nova server-detail read-back (stub)

End-to-end host validation of [cloud-storage#524](https://github.com/cobaltcore-dev/cloud-storage/issues/524)
(finding **F1**): `GET /v2.1/servers/{id}` and `/servers/detail` now return `key_name` and
`security_groups`, so a repeated `terraform apply` is a **no-op** instead of replacing the instance.

**Topology.** Lightstack in **stub mode** (config default) run directly as the built binary against one
PostgreSQL container, on a single Ubuntu host. All the changes are DB + HTTP JSON, so stub mode
exercises them fully — no libvirt/KVM/Ceph. The binary **auto-migrates and seeds** on startup
(admin/secret, project `default`, flavors, the default security group).

Stub mode still allocates **real Neutron ports**: `main.go` calls `InitHypervisor()` unconditionally and
`NewVMManager(uri, "stub")` returns a non-nil stub manager, so the boot goroutine runs and
`AllocatePortForInstance` writes the `ports` and `port_security_groups` rows exactly as in real mode —
only the TAP/bridge work is skipped. That is what makes §2.7 possible without libvirt.

**Two shells.** **T1** runs the Lightstack server in the foreground (leave it running); **T2** runs the
client commands (OpenStack CLI, `curl`, Terraform). Postgres runs as a background container.

| Validates | Spec | Section |
|---|---|---|
| `key_name` round-trips on show + list-detail | US1 / FR-001..003 | §2.1 |
| `key_name` is `null` when no keypair | US1 / FR-001 | §2.2 |
| `security_groups` defaults to `[{"name":"default"}]` | US2 / FR-004/006 | §2.3 |
| Unknown key_name / unknown security group → `400` | FR-002a / FR-005a | §2.4 |
| Repeated `terraform apply` is a no-op | SC-001 (the F1 fix) | §2.5 |
| Per-project `default` SG ensured on demand | FR-006a | §2.6 (optional) |
| The requested SG reaches the VM's **port** (what enforcement reads) | cloud-storage#579 create path | §2.7 |

---

## 0. Prerequisites

**A container engine — Docker or Podman — must already be installed** (this guide will not install one;
that's the host operator's setup). The block checks for it, then installs the lightweight client tools.
Idempotent — re-running skips anything already present. Groups: apt packages (build tools, OpenStack
CLI, jq); Go 1.26.2 (from the official tarball, since Ubuntu's `golang` is older, symlinked to
`/usr/local/bin`); Terraform (HashiCorp apt repo).

```bash
command -v docker >/dev/null 2>&1 || command -v podman >/dev/null 2>&1 || echo "PRECONDITION: install Docker or Podman first (this guide will not)"
sudo apt-get update
sudo apt-get install -y git make build-essential curl gnupg lsb-release jq python3-openstackclient
echo "Go 1.26.x — installing only if missing"
command -v go >/dev/null 2>&1 && go version | grep -q 'go1.26' || { curl -fsSL https://go.dev/dl/go1.26.2.linux-amd64.tar.gz -o /tmp/go.tgz && sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz; }
sudo ln -sf /usr/local/go/bin/go /usr/local/bin/go
echo "Terraform — installing only if missing"
command -v terraform >/dev/null 2>&1 || { curl -fsSL https://apt.releases.hashicorp.com/gpg | sudo gpg --batch --yes --dearmor -o /usr/share/keyrings/hashicorp-archive-keyring.gpg && echo "deb [signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com $(lsb_release -cs) main" | sudo tee /etc/apt/sources.list.d/hashicorp.list && sudo apt-get update && sudo apt-get install -y terraform; }
go version && terraform version && openstack --version && jq --version && { docker --version || podman --version; }
```

**Pass =** every version line prints — Go `go1.26.x`, Terraform, OpenStack CLI, jq, and Docker **or**
Podman. `go` is symlinked into `/usr/local/bin`, so it's on `PATH` in any shell without editing your
profile. If neither Docker nor Podman is present, install one first — a host precondition, out of scope
for this guide.

---

## 1. Setup steps

**Every block runs from the root of your Lightstack checkout** — `cd` there once in each shell you use
(T1 and T2). The few steps that need a subdirectory `cd` into it relatively and return to the root.

Three blocks, and it cannot be fewer: §1.2 runs the server in the **foreground** in T1, so everything
after it needs the second shell.

### 1.1 Clean slate, build, database, storage (any shell)

One block, safe to re-run — it is also how you **restart**: `make db-down` drops the Postgres container
and with it all DB state (servers, keypairs, images, projects), and the `rm`s drop the node-local image
files and the Terraform state, so the run starts fresh (Lightstack re-migrates and re-seeds on startup).
That clears the duplicate servers/keypairs/images that accumulate from re-running individual steps.
On a first run the removals are no-ops.

`make db-up` starts the `lightstack-postgres` container the default config expects (db `lightstack`, user
`lightstack`, password `secret`, port 5432). The `make db-*` targets invoke `docker`; on a Podman host use
the `podman-docker` shim so `docker` resolves to Podman (or run the equivalent `podman` commands).
Local-mode Glance writes to `/var/lib/lightstack/images`, a root-owned node-local dir (SPEC-011) — the
binary runs as your user, so it is made writable here, before the server starts, or the §1.3 image
upload returns `503 … permission denied`.

```bash
make db-down
sudo rm -rf /var/lib/lightstack/images 2>/dev/null || true
rm -f test/terraform/compat/terraform.tfstate test/terraform/compat/terraform.tfstate.backup
rm -f /tmp/spec017-key.pem /tmp/cirros.img
make build && ls -l bin/lightstack
make db-up && docker exec lightstack-postgres pg_isready -U lightstack -d lightstack
sudo mkdir -p /var/lib/lightstack/images && sudo chown -R "$USER" /var/lib/lightstack
```

**Pass =** `go build … -o bin/lightstack` succeeds, `bin/lightstack` is listed, and `pg_isready` prints
`… accepting connections`.

### 1.2 Start Lightstack — **Shell T1 (server)**, leave running

Uses the default `config/lightstack.yaml` (all services stub, DB
`postgres://lightstack:secret@localhost:5432/lightstack`). It runs migrations and seeds on startup.

```bash
./bin/lightstack
```

**Pass =** logs show `Migrations applied successfully`, each service `initialized`, and no
`bind: address already in use`. Leave this shell running; switch to **T2** for everything below.

### 1.3 Authenticate + create the `cirros` image — **Shell T2 (client)**

The seed creates admin/project/flavors but **no image**; §2 and the Terraform scenario need one named
`cirros`. Stub/local Glance stores a tiny dummy file (no VM boots).

```bash
export OS_AUTH_URL=http://localhost:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
openstack token issue -f value -c id
head -c 1024 /dev/urandom > /tmp/cirros.img
openstack image show cirros >/dev/null 2>&1 || openstack image create cirros --disk-format qcow2 --container-format bare --file /tmp/cirros.img --public -f value -c name
openstack image list -f value -c Name | grep -c '^cirros$'
```

**Pass =** `token issue` prints a token id, and the final count is **exactly `1`**. The `image show ||
create` guard keeps this idempotent — Glance allows duplicate names, and the §2.5 Terraform image
data source fails if more than one `cirros` exists. Keep this **T2** shell — the `OS_*` env persists
here for all of §2.

---

## 2. Test steps

All of §2 runs in **T2** (the env from §1.3 must be set). Each step states its expected outcome.

### 2.1 `key_name` round-trips on show + list-detail (US1)

`openstack keypair create` with no public key makes Lightstack generate one. Boot via `curl` — the CLI's
`server create` insists on network allocation (`--nic none` needs microversion 2.37 and Lightstack does not
accept the resulting `networks:"none"`); a plain boot request with no `networks` key is the clean path.
Then read back through the **CLI** (`server show`) *and* the **list-detail** endpoint.

```bash
openstack keypair show spec017-key >/dev/null 2>&1 || openstack keypair create spec017-key > /tmp/spec017-key.pem
TOKEN=$(openstack token issue -f value -c id)
curl -s -o /dev/null -w 'create HTTP %{http_code}\n' -X POST http://localhost:8774/v2.1/servers -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d '{"server":{"name":"spec017-kn","flavorRef":"m1.small","key_name":"spec017-key"}}'
openstack server show spec017-kn -f json | jq -c '{key_name, security_groups}'
curl -s http://localhost:8774/v2.1/servers/detail -H "X-Auth-Token: $TOKEN" | jq -c '.servers[] | select(.name=="spec017-kn") | {key_name, security_groups}'
```

**Expect:** create `HTTP 202`; `server show` prints `{"key_name":"spec017-key","security_groups":[{"name":"default"}]}`; the list-detail line prints the **same** — proving both the single-server and list endpoints read the fields back.

### 2.2 `key_name` is `null` with no keypair (US1)

```bash
TOKEN=$(openstack token issue -f value -c id)
curl -s -o /dev/null -w 'create HTTP %{http_code}\n' -X POST http://localhost:8774/v2.1/servers -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d '{"server":{"name":"spec017-nokey","flavorRef":"m1.small"}}'
openstack server show spec017-nokey -f json | jq '.key_name'
```

**Expect:** create `HTTP 202`; the last line prints `null` (the field is present, value null).

### 2.3 `security_groups` defaults to `default` (US2 / FR-006)

Re-uses the no-security-group server from §2.2.

```bash
openstack server show spec017-nokey -f json | jq -r '.security_groups[].name'
```

**Expect:** prints `default` (a non-empty list `[{"name":"default"}]`, not empty).

### 2.4 Create-time validation → `400` (FR-002a, FR-005a)

Uses raw `curl` on Nova because the CLI resolves keypair/security-group names client-side before the
boot call — `curl` drives the server-side validation directly.

```bash
TOKEN=$(openstack token issue -f value -c id)
curl -s -w '\nHTTP %{http_code}\n' -X POST http://localhost:8774/v2.1/servers -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d '{"server":{"name":"spec017-badkey","flavorRef":"m1.small","key_name":"nope"}}'
curl -s -w '\nHTTP %{http_code}\n' -X POST http://localhost:8774/v2.1/servers -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d '{"server":{"name":"spec017-badsg","flavorRef":"m1.small","security_groups":[{"name":"nope"}]}}'
openstack server list -f value -c Name | grep -c spec017-bad
```

**Expect:** first curl → body `{"badRequest":{"message":"Invalid key_name provided.","code":400}}` and
`HTTP 400`; second → `{"badRequest":{"message":"Security group nope not found.","code":400}}` and
`HTTP 400`; the final count prints `0` (neither instance was created).

### 2.5 Repeated `terraform apply` is a no-op — the F1 fix (SC-001)

This is the headline. `terraform init` downloads the provider (needs internet). The manifest sets
`key_pair` and `security_groups` on the instance — exactly what used to force a replace.

```bash
cd test/terraform/compat
terraform init -input=false
terraform apply -auto-approve
terraform plan -detailed-exitcode
echo "plan exit: $?"
cd ../../..
```

**Expect:** `apply` ends with `Apply complete! Resources: 9 added`; then `terraform plan` prints
`No changes. Your infrastructure matches the configuration.` and `plan exit: 0`. **Exit `2` = FAIL**
(a resource still wants replacing → the read-back regressed).

### 2.6 Per-project `default` SG ensured on demand (FR-006a) — optional

Proves an API-created project (which has no seeded `default` group) still accepts
`security_groups=["default"]` — Lightstack creates the group on demand. Uses a **unique** project name each
run (`spec017-proj-<epoch>`) and drives everything by the returned project **ID** (`$PROJ`): Lightstack returns
`500` if you re-create an existing project name (a create-idempotency gap), so a fresh name keeps the
step re-runnable. (Also covered by the unit tests, and host-verified.)

```bash
PROJ=$(openstack project create "spec017-proj-$(date +%s)" -f value -c id)
ADMIN_ROLE=$(openstack role list -f value -c ID -c Name | grep -w admin | head -1 | cut -d' ' -f1)
openstack role add --project "$PROJ" --user admin "$ADMIN_ROLE"
TOKEN2=$(openstack --os-project-id "$PROJ" token issue -f value -c id)
curl -s -w '\nHTTP %{http_code}\n' -X POST http://localhost:8774/v2.1/servers -H "X-Auth-Token: $TOKEN2" -H 'Content-Type: application/json' -d '{"server":{"name":"spec017-fresh","flavorRef":"m1.small","security_groups":[{"name":"default"}]}}'
openstack --os-project-id "$PROJ" security group list -f value -c Name
openstack --os-project-id "$PROJ" server list -f value -c ID | xargs -r openstack --os-project-id "$PROJ" server delete
openstack project delete "$PROJ"
```

**Expect:** the curl prints `HTTP 202` (create accepted — the group was ensured), and the security
group list prints `default` (auto-created on demand). The last two lines clean up the throwaway
project (cascading its server + SG).

### 2.7 The requested security group reaches the VM's port (cloud-storage#579, create path)

The read-back in §2.1–§2.3 comes from `server_security_groups`. What actually filters traffic is
`port_security_groups` — `portIngressRules` joins it for the iptables dataplane,
`fetchSecurityGroupRulesForPort` for eBPF. Before this change `AllocatePortForInstance` hardcoded the
project `default` group, so a server reporting `spec017-web` was filtered by `default`. This step proves
the two now agree.

Needs a network **with a subnet** (port allocation fails without one) and a `networks` entry in the boot
request. The port is written by the async boot goroutine, so the check polls for it. The creates pass
`-f value -c id` deliberately — see the troubleshooting note on `security group create`.

Verified on the sandbox host 2026-09-17 against a scratch DB, both directions: the pre-fix binary
(built from `HEAD`) reported `spec017-web` on a port carrying the `default` id; the fixed binary put
`spec017-web` on the port.

```bash
TOKEN=$(openstack token issue -f value -c id)
openstack security group show spec017-web >/dev/null 2>&1 || openstack security group create spec017-web -f value -c id >/dev/null
openstack network show spec017-net >/dev/null 2>&1 || openstack network create spec017-net -f value -c id >/dev/null
openstack subnet show spec017-sub >/dev/null 2>&1 || openstack subnet create spec017-sub --network spec017-net --subnet-range 10.30.0.0/24 -f value -c id >/dev/null
WEB=$(openstack security group show spec017-web -f value -c id)
DEF=$(openstack security group show default -f value -c id)
NET=$(openstack network show spec017-net -f value -c id)
SRV=$(curl -s -X POST http://localhost:8774/v2.1/servers -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d "{\"server\":{\"name\":\"spec017-sgport\",\"flavorRef\":\"m1.small\",\"security_groups\":[{\"name\":\"spec017-web\"}],\"networks\":[{\"uuid\":\"$NET\"}]}}" | jq -r '.server.id')
SRV2=$(curl -s -X POST http://localhost:8774/v2.1/servers -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d "{\"server\":{\"name\":\"spec017-sgdef\",\"flavorRef\":\"m1.small\",\"networks\":[{\"uuid\":\"$NET\"}]}}" | jq -r '.server.id')
for i in 1 2 3 4 5 6 7 8 9 10; do PORTSG=$(curl -s "http://localhost:9696/v2.0/ports?device_id=$SRV" -H "X-Auth-Token: $TOKEN" | jq -r '.ports[0].security_groups[]?'); test -n "$PORTSG" && break; sleep 1; done
for i in 1 2 3 4 5 6 7 8 9 10; do PORTSG2=$(curl -s "http://localhost:9696/v2.0/ports?device_id=$SRV2" -H "X-Auth-Token: $TOKEN" | jq -r '.ports[0].security_groups[]?'); test -n "$PORTSG2" && break; sleep 1; done
echo "requested-group port: $PORTSG"
echo "spec017-web:          $WEB"
echo "no-group port:        $PORTSG2"
echo "default:              $DEF"
echo "server says:" && openstack server show "$SRV" -f json | jq -r '.security_groups[].name'
```

**Expect:** `requested-group port` equals `spec017-web` and is **not** `default`, and `server says`
prints `spec017-web` — the port and the read-back name the same group, which is the fix. The negative
control `no-group port` equals `default` — a server that requested nothing still falls back to the
project default. **FAIL** = `requested-group port` prints the `default` id while the server reports
`spec017-web` (the mismatch the reviewer flagged).

**Scope.** This covers the **create** path only. `os-addSecurityGroup` / `os-removeSecurityGroup` and
`os-interface` still write `server_security_groups` without touching the port; that half, plus the
per-port filter refresh and a real-mode dataplane run, stays in
[cloud-storage#579](https://github.com/cobaltcore-dev/cloud-storage/issues/579). §2.5's Terraform run
exercises the same path (its instance uses a non-default group on a tenant network) if you want a
second confirmation.

---

## 3. Teardown steps

In **T1** press `Ctrl-C` to stop Lightstack, then run this in **T2**. (To *restart* mid-run rather than
tear down for good, re-run **§1.1** instead — it wipes all DB state without needing Lightstack running.)

```bash
(cd test/terraform/compat && terraform destroy -auto-approve) 2>/dev/null || true
openstack server delete spec017-kn spec017-nokey spec017-sgport spec017-sgdef 2>/dev/null || true
openstack port list --network spec017-net -f value -c ID 2>/dev/null | xargs -r openstack port delete 2>/dev/null || true
openstack subnet delete spec017-sub 2>/dev/null || true
openstack network delete spec017-net 2>/dev/null || true
openstack security group delete spec017-web 2>/dev/null || true
openstack keypair delete spec017-key 2>/dev/null || true
openstack image delete cirros 2>/dev/null || true
rm -f /tmp/spec017-key.pem /tmp/cirros.img
make db-down
```

**Pass =** no `spec017-*` servers, networks or security groups, no `lightstack-postgres` container,
`/tmp/spec017-*` removed.

To also remove the tools §0 installed (skip if you want to keep them):
`sudo rm -rf /usr/local/go && sudo apt-get remove -y terraform python3-openstackclient && sudo rm -f /usr/share/keyrings/hashicorp-archive-keyring.gpg /etc/apt/sources.list.d/hashicorp.list`

---

## Troubleshooting

- **`bind: address already in use` in T1** — another process holds 35357/8774/9292. Stop it, or run Lightstack
  on a host where those ports are free (this runbook assumes they are).
- **`openstack: command not found` / auth errors** — re-run the §1.3 `export OS_*` block in T2; the
  domain must be `Default` (capitalised).
- **`make db-up` → "permission denied" / "cannot connect to the Docker daemon"** — your user can't reach
  the engine. Use rootless Docker/Podman, or add yourself to the `docker` group and re-login
  (`newgrp docker`), or prefix the `make db-*` lines with `sudo`. This guide does not configure the
  engine.
- **`make db-up` → "0.0.0.0:5432: bind: address already in use"** — a Postgres already holds 5432 (e.g.
  a pre-existing dev container). Find it with `docker ps --filter publish=5432` and remove it
  (`docker rm -f <name>`), then re-run §1.1. `make db-down` also clears the dead `lightstack-postgres` the
  failed run leaves behind. A fresh host won't hit this.
- **`terraform init` fails** — it needs internet to fetch `terraform-provider-openstack ~> 3.0`.
- **`openstack security group create` prints `'NoneType' object is not iterable`** — the group **is**
  created; the CLI's table renderer trips over Lightstack's create response, which omits
  `security_group_rules` (and `project_id`, `revision_number`, `tags`). Pre-existing and unrelated to
  #524 — a Neutron API-fidelity gap. Adding `-f value -c id` (as §2.7 does) sidesteps the renderer.
- **§2.7 `port group` prints nothing** — no port was allocated. Either the boot request carried no
  `networks` entry (check the `-d` payload interpolated `$NET`), or the network has no subnet
  (`AllocatePortForInstance` needs one to allocate an IP — re-run the `subnet create` line), or the
  goroutine errored. Check T1 for `Failed to allocate port from Neutron` / `no subnet found`.
- **A field renders oddly under the CLI** — assert against the raw API instead:
  `curl -s http://localhost:8774/v2.1/servers/<id> -H "X-Auth-Token: $TOKEN" | jq '.server.key_name, .server.security_groups'`.
- **`503` + `permission denied … /var/lib/lightstack/images` on image create** — the node-local storage dir
  isn't writable by the Lightstack user. `sudo mkdir -p /var/lib/lightstack/images && sudo chown -R "$USER" /var/lib/lightstack` (this is §1.1), then retry.
- **`openstack server create` errors with "microversion 2.37 … required" / creates nothing** — the CLI
  wants network allocation. This guide boots via `curl` with no `networks` key instead; do **not** use
  `--nic none` (Lightstack does not accept the `networks:"none"` the CLI sends at 2.37).
- **`terraform apply` → "Your query returned more than one result" on `data … img`** — duplicate
  `cirros` images (Glance allows duplicate names; re-running §1.3 without the guard makes more). Reduce
  to one and re-run apply:
  `openstack image list -f value -c ID -c Name | grep -w cirros | cut -d' ' -f1 | xargs -r openstack image delete` then re-run the §1.3 `image show || create` line.
