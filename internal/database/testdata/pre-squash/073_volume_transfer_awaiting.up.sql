-- SPEC-025 follow-up — a volume with a pending transfer is held in upstream's awaiting-transfer state.
--
-- Transfer create now moves the volume from available to awaiting-transfer, and accept moves it only out
-- of that state (cinder/db/api.py transfer_create, transfer_accept). Transfers created before this
-- release left their volume available, so they could no longer be accepted; this puts their volumes
-- where a create now leaves them.
--
-- Only volumes that are available: a volume in any other state could not be offered by today's create
-- either, and accepting it stays refused. A volume with two pending transfers — possible before, and
-- the defect this closes — is held once; the first accept moves it and the other then matches nothing.
UPDATE volumes SET status = 'awaiting-transfer', updated_at = now()
 WHERE status = 'available'
   AND id IN (SELECT volume_id FROM volume_transfers WHERE accepted = false);
