-- Remove Default domain seeding
-- Must NULL domain_id references before deleting to avoid cascading user/project deletion
UPDATE users SET domain_id = NULL WHERE domain_id = '00000000-0000-0000-0000-000000000100';
UPDATE projects SET domain_id = NULL WHERE domain_id = '00000000-0000-0000-0000-000000000100';
DELETE FROM domains WHERE name = 'Default';
