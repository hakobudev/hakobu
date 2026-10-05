// The panel's way to Cloudflare's OAuth token endpoint when its own
// server's network is challenged there (cloudflare_oauth.go): it runs
// inside Cloudflare, which its bot protection doesn't challenge. It takes
// only the panel's requests (SECRET, made anew each time the panel starts)
// for the panel's OAuth client (CLIENT_ID), passes them to the token
// endpoint and the answer back, and keeps and logs nothing.

const TOKEN_URL = "https://dash.cloudflare.com/oauth2/token";
const GRANTS = ["authorization_code", "refresh_token"];

// same compares in time that doesn't depend on where a and b differ.
function same(a, b) {
	if (typeof a !== "string" || typeof b !== "string" || a.length !== b.length) return false;
	let diff = 0;
	for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
	return diff === 0;
}

export async function relay(req, env) {
	if (req.method !== "POST") return new Response(null, { status: 405 });
	if (!env.SECRET || !same(req.headers.get("authorization"), "Bearer " + env.SECRET)) {
		return new Response(null, { status: 401 });
	}
	const form = new URLSearchParams(await req.text());
	if (form.get("client_id") !== env.CLIENT_ID || !GRANTS.includes(form.get("grant_type"))) {
		return new Response(null, { status: 400 });
	}
	const r = await fetch(TOKEN_URL, {
		method: "POST",
		headers: { "content-type": "application/x-www-form-urlencoded", accept: "application/json" },
		body: form.toString(),
	});
	const headers = { "content-type": r.headers.get("content-type") || "application/json", "cache-control": "no-store" };
	if (r.headers.get("cf-mitigated")) headers["cf-mitigated"] = r.headers.get("cf-mitigated");
	return new Response(r.body, { status: r.status, headers });
}

export default { fetch: relay };
