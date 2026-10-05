// Tests of the token relay Worker: `just test-js`.
import { test, expect } from "bun:test";
import { relay } from "./token_relay.js";

const env = { SECRET: "s3cret", CLIENT_ID: "cid" };

function post(body, auth = "Bearer s3cret", method = "POST") {
	return new Request("https://relay.example.workers.dev/", {
		method, headers: auth ? { authorization: auth } : {}, body: method === "POST" ? body : undefined,
	});
}

test("passes the panel's requests to the token endpoint and the answer back", async () => {
	let sent;
	globalThis.fetch = async (url, init) => {
		sent = { url, body: init.body };
		return new Response('{"access_token":"at"}', { status: 200, headers: { "content-type": "application/json" } });
	};
	const r = await relay(post("grant_type=refresh_token&client_id=cid&refresh_token=rt"), env);
	expect(r.status).toBe(200);
	expect(await r.text()).toBe('{"access_token":"at"}');
	expect(r.headers.get("cache-control")).toBe("no-store");
	expect(sent.url).toBe("https://dash.cloudflare.com/oauth2/token");
	expect(new URLSearchParams(sent.body).get("refresh_token")).toBe("rt");
});

test("refuses anyone else, other clients and other grants", async () => {
	globalThis.fetch = async () => { throw new Error("must not be called"); };
	expect((await relay(post("grant_type=refresh_token&client_id=cid", "Bearer wrong"), env)).status).toBe(401);
	expect((await relay(post("grant_type=refresh_token&client_id=cid", ""), env)).status).toBe(401);
	expect((await relay(post("grant_type=refresh_token&client_id=other"), env)).status).toBe(400);
	expect((await relay(post("grant_type=client_credentials&client_id=cid"), env)).status).toBe(400);
	expect((await relay(post("", "Bearer s3cret", "GET"), env)).status).toBe(405);
	expect((await relay(post("grant_type=refresh_token&client_id=cid"), { CLIENT_ID: "cid" })).status).toBe(401);
});

test("says when it's challenged too", async () => {
	globalThis.fetch = async () => new Response("<html>", { status: 403, headers: { "cf-mitigated": "challenge", "content-type": "text/html" } });
	const r = await relay(post("grant_type=refresh_token&client_id=cid&refresh_token=rt"), env);
	expect(r.status).toBe(403);
	expect(r.headers.get("cf-mitigated")).toBe("challenge");
});
