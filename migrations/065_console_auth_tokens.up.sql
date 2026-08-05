-- Migration 065: real console tokens (SPEC-021 / cloud-storage#534, finding F1).
--
-- This table is the STORE OF RECORD for console access. The proxy reads a
-- projection of it from /var/lib/o3k/console-tokens/ (one file per live token,
-- "<token>: <host>:<port>"), because websockify's token-file format cannot carry
-- an expiry, an instance or a project — yet all three are required:
--   * expires_at  -> FR-010, and it is enforced by DELETING the projection entry,
--                    since the proxy performs no expiry check of its own;
--   * instance_id -> FR-014, revoke every token of an instance whose console
--                    endpoint may have moved (stop/start/reboot/suspend/resume),
--                    otherwise a stale token can resolve to a port libvirt has
--                    since reassigned to a DIFFERENT instance;
--   * project_id  -> FR-013, project-scoped issuance with an admin override.
--
-- Deliberately NO console_type column: FR-025 refuses every type but novnc, so
-- storing one would be future-proofing (Article VII / YAGNI). The API response
-- echoes the requested type from the request body.
CREATE TABLE IF NOT EXISTS console_auth_tokens (
    token       TEXT PRIMARY KEY,
    instance_id UUID        NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    project_id  UUID        NOT NULL,
    host        TEXT        NOT NULL,
    port        INTEGER     NOT NULL CHECK (port > 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL
);

-- Revocation is a delete-by-instance on every endpoint-moving operation.
CREATE INDEX IF NOT EXISTS idx_console_auth_tokens_instance
    ON console_auth_tokens (instance_id);

-- The reconciler sweeps by expiry; keep it an indexed delete, not a scan.
CREATE INDEX IF NOT EXISTS idx_console_auth_tokens_expires
    ON console_auth_tokens (expires_at);

-- Drop two dead columns from migration 005. Both were written on every console
-- request and neither was ever usable:
--   * console_vnc_port     was 5900 + (hash(instance_id) mod 1000) — fabricated,
--                          never read back from libvirt. The real port is now
--                          read per request (FR-004), so nothing stores it.
--   * console_vnc_password was generated and stored but NEVER applied to a
--                          domain — the graphics element carries no passwd
--                          attribute. Dead on arrival.
ALTER TABLE instances DROP COLUMN IF EXISTS console_vnc_port;
ALTER TABLE instances DROP COLUMN IF EXISTS console_vnc_password;
