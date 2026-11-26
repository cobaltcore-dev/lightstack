-- Reverse migration 071.
DELETE FROM services WHERE id = '00000000-0000-0000-0000-000000000017'; -- cascades to its endpoint
