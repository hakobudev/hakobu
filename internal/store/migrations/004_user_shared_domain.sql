-- Users the admin lets take addresses in the panel's shared domain
-- (HAKOBU_SHARED_DOMAIN), with projects in the panel's Cloudflare account.
ALTER TABLE users ADD COLUMN shared_domain INTEGER NOT NULL DEFAULT 0;
