-- SQLite cannot add NOT NULL to these existing tables without rebuilding
-- referenced tables. Keep the PostgreSQL invariant with insert/update guards.
CREATE TRIGGER volumes_project_id_not_null_insert
BEFORE INSERT ON volumes
WHEN NEW.project_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'volumes.project_id may not be null');
END;
CREATE TRIGGER volumes_project_id_not_null_update
BEFORE UPDATE OF project_id ON volumes
WHEN NEW.project_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'volumes.project_id may not be null');
END;
CREATE TRIGGER ports_project_id_not_null_insert
BEFORE INSERT ON ports
WHEN NEW.project_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'ports.project_id may not be null');
END;
CREATE TRIGGER ports_project_id_not_null_update
BEFORE UPDATE OF project_id ON ports
WHEN NEW.project_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'ports.project_id may not be null');
END;
