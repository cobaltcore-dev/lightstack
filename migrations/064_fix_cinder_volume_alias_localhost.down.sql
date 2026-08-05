-- Revert: restore the original localhost seed for the cinder `volume` alias
-- endpoint (service ...015), as migration 054 created it.
UPDATE endpoints SET url = 'http://localhost:8776/v3' WHERE service_id = '00000000-0000-0000-0000-000000000015';
