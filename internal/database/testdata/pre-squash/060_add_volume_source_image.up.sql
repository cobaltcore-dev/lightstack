-- SPEC-009 (WS-0 / WS-5): record the image a volume was created from.
-- Set for copy-on-write (RBD) clones (which pin the source image) and for
-- boot-from-volume; left NULL for independent local copies that do not pin.
ALTER TABLE volumes ADD COLUMN IF NOT EXISTS source_image_id UUID;
CREATE INDEX IF NOT EXISTS idx_volumes_source_image_id ON volumes(source_image_id);
