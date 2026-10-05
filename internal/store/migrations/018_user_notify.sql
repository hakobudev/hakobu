-- Where a user's own alerts go (their apps' failed deploys, crashes,
-- backups): an address confirmed in the panel's Cloudflare account, which
-- sends them. '' for an admin: the panel's address; for anyone else: none.
ALTER TABLE users ADD COLUMN notify_email TEXT NOT NULL DEFAULT '';
