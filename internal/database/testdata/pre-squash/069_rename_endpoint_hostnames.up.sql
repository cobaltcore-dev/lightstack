-- Rewrite endpoint URLs left on the pre-rename catalog host (cobaltcore-dev/cloud-storage#575).
--
-- Migration 052 seeded endpoints.url with the product's Docker hostname, and the rename
-- edited 052 in place. That reaches fresh databases only: golang-migrate is version-tracked,
-- so an install already past version 52 keeps its original rows. substituteURLTemplates
-- (internal/keystone/auth.go) matches only the current host token, so those rows pass
-- through untouched and the catalog advertises a hostname that need not resolve.
--
-- Matched on the '//<host>:' authority rather than by service_id, so endpoints an operator
-- created through the API are fixed too. A no-op on a fresh database, where 052 has already
-- written the current host.
UPDATE endpoints
SET url = REPLACE(url, '//o3k:', '//lightstack:')
WHERE url LIKE '%//o3k:%';
