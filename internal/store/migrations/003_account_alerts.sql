-- What hakobu set up in a client's Cloudflare account to alert its owner
-- about the projects there, so alerts don't all go through the panel's
-- account (which confirms at most 200 addresses). Emails go to email, an
-- address confirmed in the account (added_address: hakobu added it), from
-- alerts@<sender_domain>; routed_domain is the subdomain hakobu turned
-- Email Routing on for ('' if the zone had it). The watchdog is a Worker
-- there (script, '' while no app there has an address) checking the
-- account's apps, with its KV namespace, the targets it was last deployed
-- with (JSON) and the D1 database of their history ('' without one).
CREATE TABLE account_alerts (
	cloudflare_account_id INTEGER PRIMARY KEY,
	email TEXT NOT NULL,
	added_address INTEGER NOT NULL DEFAULT 0,
	sender_domain TEXT NOT NULL,
	zone_id TEXT NOT NULL,
	routed_domain TEXT NOT NULL DEFAULT '',
	script TEXT NOT NULL DEFAULT '',
	kv_namespace_id TEXT NOT NULL DEFAULT '',
	targets TEXT NOT NULL DEFAULT '',
	d1_database_id TEXT NOT NULL DEFAULT ''
);
