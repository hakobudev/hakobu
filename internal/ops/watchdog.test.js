// Tests of the watchdog Worker: `just test-js`.
import { test, expect } from "bun:test";
import { check, due, PER_RUN } from "./watchdog.js";

const PANEL = { name: "panel", url: "https://hakobu.example.com/healthz", need: "ok" };
const SHOP = { name: "app:shop", url: "https://shop.example.com/", need: "any" };

// env answers each URL with the statuses in turn (the last one repeats).
function env(answers, targets = [PANEL, SHOP]) {
	const kv = new Map(), sent = [], rows = [];
	const calls = new Map();
	globalThis.fetch = async (url) => {
		const list = answers[url] ?? [200];
		const i = calls.get(url) ?? 0;
		calls.set(url, i + 1);
		const s = list[Math.min(i, list.length - 1)];
		if (s === "timeout") { const e = new Error("t"); e.name = "TimeoutError"; throw e; }
		return new Response(s === 200 ? "ok" : "bad", { status: s });
	};
	const e = {
		sent, kv, rows,
		TARGETS: JSON.stringify(targets), FROM: "alerts@example.com", TO: "me@example.org",
		STATE: {
			async get(k, t) { const v = kv.get(k); return v == null ? null : (t === "json" ? JSON.parse(v) : v); },
			async put(k, v) { kv.set(k, v); }, async delete(k) { kv.delete(k); },
		},
		EMAIL: { async send(m) { sent.push(m.subject + " | " + m.text.split("\n")[0]); } },
		DB: {
			prepare(sql) { return { bind: (...args) => ({ sql, args }) }; },
			async batch(st) { rows.push(...st); },
		},
	};
	e.answer = (a) => { answers = a; calls.clear(); };
	return e;
}
const nosleep = async () => {};

test("a blip that passes on retry isn't an outage", async () => {
	const e = env({ [PANEL.url]: [502, 200] });
	await check(e, 0, nosleep);
	expect(e.sent).toEqual([]);
	expect(e.kv.size).toBe(0);
});

test("an outage is mailed once, again after 6h, and its end", async () => {
	const e = env({ [PANEL.url]: [530] });
	await check(e, 0, nosleep);
	await check(e, 60_000, nosleep);
	await check(e, 6 * 3600_000, nosleep);
	expect(e.sent.length).toBe(2);
	expect(e.sent[0]).toContain("The panel is unreachable");
	expect(e.sent[0]).toContain("530: the tunnel isn't connected");
	expect(e.sent[1]).toContain("still unreachable");
	e.answer({});
	await check(e, 6 * 3600_000 + 125 * 60_000, nosleep);
	expect(e.sent[2]).toContain("The panel answers again");
	expect(e.sent[2]).toContain("for 8 h 5 min");
	expect(e.kv.size).toBe(0);
});

test("an app unreachable while the panel answers is mailed on its own", async () => {
	const e = env({ [SHOP.url]: ["timeout"] });
	await check(e, 0, nosleep);
	expect(e.sent).toEqual([expect.stringContaining("shop is unreachable")]);
	expect(e.sent[0]).toContain("no answer within 10 s");
	e.answer({ [SHOP.url]: [404] }); // any answer but a server error
	await check(e, 60_000, nosleep);
	expect(e.sent[1]).toContain("shop answers again");
});

test("with the panel down, its email covers the apps", async () => {
	const e = env({ [PANEL.url]: [530], [SHOP.url]: [530] });
	await check(e, 0, nosleep);
	expect(e.sent).toEqual([expect.stringContaining("The panel is unreachable")]);
});

test("every check is recorded, and old ones pruned on the hour", async () => {
	const e = env({ [SHOP.url]: [503] });
	await check(e, 90_000, nosleep);
	expect(e.rows.map((r) => r.args)).toEqual([
		["panel", 60, 1, expect.any(Number), 200],
		["app:shop", 60, 0, expect.any(Number), 503],
	]);
	e.rows.length = 0;
	await check(e, 3600_000, nosleep);
	expect(e.rows.at(-1).sql).toContain("DELETE FROM checks");
});

test("the panel's outage as v0.9 kept it still ends with an email", async () => {
	const e = env({});
	e.kv.set("down", JSON.stringify({ since: 0, mailed: 0 }));
	await check(e, 600_000, nosleep);
	expect(e.sent).toEqual([expect.stringContaining("The panel answers again")]);
	expect(e.kv.size).toBe(0);
});

test("apps going down together are one email, and so is their return", async () => {
	const CART = { name: "app:cart", label: "cart", url: "https://cart.example.com/", need: "any", server: "t2" };
	const e = env({ [SHOP.url]: [530], [CART.url]: [530] }, [SHOP, CART]);
	await check(e, 0, nosleep);
	expect(e.sent).toEqual([expect.stringContaining("2 apps are unreachable: shop, cart")]);
	expect(e.sent[0]).toContain("shop at https://shop.example.com/: it answered HTTP 530");
	e.answer({});
	await check(e, 60_000, nosleep);
	expect(e.sent[1]).toContain("2 apps answer again");
});

test("with more targets than a run may check, the apps take turns and the panel is checked each time", () => {
	const apps = Array.from({ length: 20 }, (_, i) => ({ name: `app:${i}`, url: `https://${i}.example.com/`, need: "any" }));
	const first = due([PANEL, ...apps], 0), second = due([PANEL, ...apps], 60_000), third = due([PANEL, ...apps], 120_000);
	expect(first.length).toBe(PER_RUN);
	expect(first[0]).toBe(PANEL);
	expect(second[0]).toBe(PANEL);
	expect(second.slice(1).map((t) => t.name)).toEqual(apps.slice(PER_RUN - 1).map((t) => t.name));
	expect(third).toEqual(first);
	expect(due(apps.slice(0, PER_RUN), 0).length).toBe(PER_RUN);
});

test("without the panel, an app's email says what a 530 means", async () => {
	const e = env({ [SHOP.url]: [530] }, [SHOP]);
	e.PANEL = "https://hakobu.example.com";
	await check(e, 0, nosleep);
	expect(e.sent).toEqual([expect.stringContaining("shop is unreachable")]);
});
