-- Revert endpoint URLs to the pre-rename catalog host.
UPDATE endpoints
SET url = REPLACE(url, '//lightstack:', '//o3k:')
WHERE url LIKE '%//lightstack:%';
