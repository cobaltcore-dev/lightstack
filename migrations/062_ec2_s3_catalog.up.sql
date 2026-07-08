-- SPEC-014: EC2/S3 credentials + the `s3` (Ceph RGW) catalog entry.

-- Fast, unique lookup of an EC2 credential by its S3 access key. POST /v3/s3tokens
-- resolves RGW's incoming access key -> credential via this index.
ALTER TABLE credentials ADD COLUMN IF NOT EXISTS access_key VARCHAR(255);
CREATE UNIQUE INDEX IF NOT EXISTS idx_credentials_access_key
    ON credentials(access_key) WHERE access_key IS NOT NULL;

-- Advertise the object-store endpoint under the `s3` service type (NOT the Swift
-- `object-store` type — o3k exposes no Swift API; SPEC-014 FR-009). Uses service
-- id ...016 — ids ...010–...015 are already taken (migrations 040 and 054).
--
-- The URL below is an INERT placeholder that is NEVER served to a client. At token
-- issue time the catalog always substitutes the resolved config value
-- (glance.s3_endpoint / O3K_GLANCE_S3_ENDPOINT, wired via SetGlanceS3Endpoint — the
-- same source the S3 client uses), and omits the `s3` service entirely when RGW is
-- unconfigured. Both the DB-backed catalog and the hardcoded fallback behave this way
-- (internal/keystone/auth.go). Consequence: editing this endpoint row — e.g. with
-- `openstack endpoint set` — has no effect; configure glance.s3_endpoint instead.
-- (PR #73 review r3710751586.)
INSERT INTO services (id, type, name, description, enabled) VALUES
    ('00000000-0000-0000-0000-000000000016', 's3', 'rgw', 'S3 Object Storage (Ceph RGW)', true)
ON CONFLICT DO NOTHING;
INSERT INTO endpoints (service_id, interface, url, region) VALUES
    ('00000000-0000-0000-0000-000000000016', 'public', 'http://localhost:7480', 'RegionOne')
ON CONFLICT DO NOTHING;
