-- A database's size at each check, in bytes, to tell how fast it grows; 0
-- for a cache, or a check that got no answer.
ALTER TABLE data_checks ADD COLUMN size INTEGER NOT NULL DEFAULT 0;
