-- SQLite cannot add NOT NULL to these existing tables without rebuilding
-- referenced tables. Keep the PostgreSQL invariant with insert/update guards.
CREATE TRIGGER networks_project_id_not_null_insert
BEFORE INSERT ON networks
WHEN NEW.project_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'networks.project_id may not be null');
END;
CREATE TRIGGER networks_project_id_not_null_update
BEFORE UPDATE OF project_id ON networks
WHEN NEW.project_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'networks.project_id may not be null');
END;
CREATE TRIGGER routers_project_id_not_null_insert
BEFORE INSERT ON routers
WHEN NEW.project_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'routers.project_id may not be null');
END;
CREATE TRIGGER routers_project_id_not_null_update
BEFORE UPDATE OF project_id ON routers
WHEN NEW.project_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'routers.project_id may not be null');
END;
CREATE TRIGGER floating_ips_project_id_not_null_insert
BEFORE INSERT ON floating_ips
WHEN NEW.project_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'floating_ips.project_id may not be null');
END;
CREATE TRIGGER floating_ips_project_id_not_null_update
BEFORE UPDATE OF project_id ON floating_ips
WHEN NEW.project_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'floating_ips.project_id may not be null');
END;
