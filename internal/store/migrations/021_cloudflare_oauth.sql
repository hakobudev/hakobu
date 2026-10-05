-- A Cloudflare account connected with OAuth ("Connect with Cloudflare"):
-- api_token holds its access token, refreshed with refresh_token before
-- token_expires. r2_token is an API token for R2 alone, whose S3 keys
-- backups use: an OAuth token has none. An account connected with an API
-- token has neither, and its token's S3 keys serve.
ALTER TABLE cloudflare_accounts ADD COLUMN refresh_token TEXT NOT NULL DEFAULT '';
ALTER TABLE cloudflare_accounts ADD COLUMN token_expires TEXT NOT NULL DEFAULT '';
ALTER TABLE cloudflare_accounts ADD COLUMN r2_token TEXT NOT NULL DEFAULT '';
