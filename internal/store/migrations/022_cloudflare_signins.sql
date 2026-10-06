-- One "Connect with Cloudflare" sign-in can connect several accounts: the
-- ones it connected share its tokens under the same signin, renewed once
-- for all of them, as Cloudflare's refresh tokens work only once. '' for
-- an account with an API token.
ALTER TABLE cloudflare_accounts ADD COLUMN signin TEXT NOT NULL DEFAULT '';
UPDATE cloudflare_accounts SET signin = lower(hex(randomblob(16))) WHERE refresh_token != '';
