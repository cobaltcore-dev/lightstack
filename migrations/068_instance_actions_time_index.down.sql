-- Reversible per the Constitution's Backwards Compatibility rule. Dropping the index only makes the
-- reclamation sweep slower; nothing depends on its existence for correctness.
DROP INDEX IF EXISTS idx_instance_actions_start_time;
