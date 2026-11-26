-- Reversible per the Constitution's Backwards Compatibility rule. Nothing else reads this column, so
-- dropping it restores the prior behaviour exactly.
ALTER TABLE networks DROP COLUMN IF EXISTS external;
