DROP TABLE reservation_owners;
ALTER TABLE snapshots DROP COLUMN reserved_owner;
ALTER TABLE volumes DROP COLUMN reserved_owner;
