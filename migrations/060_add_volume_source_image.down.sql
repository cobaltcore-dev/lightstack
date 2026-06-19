DROP INDEX IF EXISTS idx_volumes_source_image_id;
ALTER TABLE volumes DROP COLUMN IF EXISTS source_image_id;
