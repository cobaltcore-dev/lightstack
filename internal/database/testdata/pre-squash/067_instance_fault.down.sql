-- Reversible per the Constitution's Backwards Compatibility rule. Nothing outside the fault view reads
-- these columns, so dropping them restores the prior behaviour exactly: no fault object is emitted.
ALTER TABLE instances DROP COLUMN IF EXISTS fault_created;
ALTER TABLE instances DROP COLUMN IF EXISTS fault_message;
ALTER TABLE instances DROP COLUMN IF EXISTS fault_code;
