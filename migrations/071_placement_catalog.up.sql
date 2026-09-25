-- Advertise the placement service, which cmd/lightstack serves on :8778. The hardcoded
-- fallback catalog always listed it, but no migration seeded it, and the fallback is
-- served only when this table cannot be read, so real catalogs never did. Horizon's
-- Hypervisors -> Resource Provider tab looks placement up in the catalog and showed
-- "Unable to retrieve providers information." without it; with it, the stub's empty
-- provider list renders as an empty table.
--
-- Service id ...017: ids ...010-...016 are taken (migrations 040, 054 and 062). The URL
-- follows the http://lightstack: convention of every other row (migrations 052 and 064);
-- the catalog rewrites it to the configured endpoint host when a token is issued.
INSERT INTO services (id, type, name, description, enabled) VALUES
    ('00000000-0000-0000-0000-000000000017', 'placement', 'placement', 'Placement Service', true)
ON CONFLICT DO NOTHING;
-- endpoints has no unique key for ON CONFLICT to act on, so the guard is explicit: a
-- second run must not add a second endpoint.
INSERT INTO endpoints (service_id, interface, url, region)
SELECT '00000000-0000-0000-0000-000000000017', 'public', 'http://lightstack:8778', 'RegionOne'
WHERE NOT EXISTS (
    SELECT 1 FROM endpoints
     WHERE service_id = '00000000-0000-0000-0000-000000000017' AND interface = 'public'
);
