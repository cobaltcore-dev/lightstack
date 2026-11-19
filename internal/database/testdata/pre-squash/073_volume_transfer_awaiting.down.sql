-- Reversible per the Constitution's Backwards Compatibility rule: back to the model without the state,
-- in which a volume with a pending transfer stays available.
UPDATE volumes SET status = 'available', updated_at = now()
 WHERE status = 'awaiting-transfer';
