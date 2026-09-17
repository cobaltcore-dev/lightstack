-- Drop the world-open SSH rule the original migration 002 seeded into the `default`
-- project's default security group (cobaltcore-dev/cloud-storage#528).
--
-- 002 was edited in place to stop seeding it, which reaches fresh databases only:
-- golang-migrate is version-tracked, so an install that already ran 002 keeps the row
-- it inserted — an `ingress IPv4 tcp 22/22` carrying neither remote_ip_prefix nor
-- remote_group_id, which both dataplanes enforce as 0.0.0.0/0 (ingressRuleSpec in
-- pkg/networking/northsouth.go emits no -s match for an empty remote, and
-- pkg/networking/security_groups_ebpf.go substitutes 0.0.0.0/0 outright). Migration
-- 069 exists for the same reason, one in-place edit earlier.
--
-- Numbered 070 on purpose, above main's 069 rather than in the 063-068 range the
-- stacked branches hold: golang-migrate applies only versions greater than the one
-- recorded, so a 064 here would never run on a database already at 69. The
-- migration-numbering cleanup at the end of the stack may renumber this file — it
-- stays correct at any number that lands after the migrations preceding it.
--
-- Matched on the exact shape 002 inserted, on that one seeded group: a rule an
-- operator created with an explicit remote carries remote_ip_prefix and is left
-- alone. A no-op on a fresh database, where the rewritten 002 never inserted it —
-- migrations run before the API serves (cmd/lightstack/main.go:114), so no rule of
-- any shape exists yet.
--
-- The seed carries no fixed id (security_group_rules.id defaults to gen_random_uuid(),
-- migration 001), so shape is the only handle. One collision remains: a rule an
-- operator created through the API with NO remote at all lands as the same row
-- (internal/neutron/ports.go stores an omitted remote as NULL) and would be removed
-- too. Accepted deliberately — it can only take away a world-open SSH rule, never
-- grant access, and reaching it requires a pre-rewrite install that already served
-- traffic. Preserving it would need provenance the schema does not carry.
DELETE FROM security_group_rules
WHERE security_group_id = '00000000-0000-0000-0000-000000000030'
  AND direction = 'ingress'
  AND ethertype = 'IPv4'
  AND protocol = 'tcp'
  AND port_range_min = 22
  AND port_range_max = 22
  AND remote_ip_prefix IS NULL
  AND remote_group_id IS NULL;
