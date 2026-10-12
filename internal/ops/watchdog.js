// hakobu's watchdog: a Worker hakobu deploys to a Cloudflare account it
// sends alerts from: the panel's, checking the panel and the apps in that
// account, and each client's connected to the panel, checking the apps
// there. Every minute it asks them from outside the server, the way their
// visitors reach them, and emails when they stop answering and when
// they're back. hakobu can't see that itself: the server, hakobu or the
// tunnel may be down, or an app's address broken while it runs fine
// inside.
//
// Bindings: TARGETS (JSON: [{name, label, url, need, server}], the panel
// first if it's watched; need is "ok" for the panel's /healthz, "2xx", or
// "any" for any answer but a server error; server names the server an app
// runs on, if not the panel's), PANEL (the panel's address), FROM and TO
// (the addresses of the emails), EMAIL (sends to TO, an address verified
// in Email Routing), STATE (KV: the outages being reported, written only
// on a change), and DB (D1, if the token allows: one row per target and
// minute, kept 30 days, which the panel shows as uptime).

const TRIES = 3;
const PAUSE_MS = 15_000; // between tries: a restart isn't an outage
const TIMEOUT_MS = 10_000;
const AGAIN_MS = 6 * 60 * 60 * 1000;
const KEEP_MS = 30 * 24 * 60 * 60 * 1000;
// PER_RUN keeps a run within the 50 requests a Worker on the free plan may
// make, three tries each: with more targets, the apps take turns, minute
// by minute, and the panel is checked every time.
export const PER_RUN = 15;

export default {
	async scheduled(event, env) {
		await check(env, Date.now());
	},
};

export async function check(env, now, sleep = (ms) => new Promise((r) => setTimeout(r, ms))) {
	const targets = due(JSON.parse(env.TARGETS), now);
	const results = await Promise.all(targets.map((t) => probeTries(t, sleep)));
	if (env.DB) await record(env.DB, targets, results, now);

	let state = (await env.STATE.get("down", "json")) || {};
	if ("since" in state) state = { panel: state }; // as v0.9 kept the panel's
	const before = JSON.stringify(state);
	const panelDown = targets[0]?.name === "panel" && !!results[0].why;
	const watchesPanel = targets[0]?.name === "panel";
	const down = [], still = [], back = [];
	for (const [i, t] of targets.entries()) {
		const { why } = results[i];
		const was = state[t.name];
		if (!why) {
			if (was) {
				delete state[t.name];
				back.push({ t, was });
			}
		} else if (t.name !== "panel" && panelDown) {
			// The panel's email covers it: the server, hakobu or the tunnel.
		} else if (!was) {
			state[t.name] = { since: now, mailed: now };
			down.push({ t, why });
		} else if (now - was.mailed >= AGAIN_MS) {
			state[t.name] = { since: was.since, mailed: now };
			still.push({ t, why, was });
		}
	}
	for (const e of [...down, ...still].filter((e) => e.t.name === "panel")) {
		await mail(env, `The panel is ${e.was ? "still " : ""}unreachable`,
			(e.was ? `Unreachable for ${minutes(now - e.was.since)}. ` : "") + panelUnreachable(e.t, e.why));
	}
	const apps = (list) => list.filter((e) => e.t.name !== "panel");
	if (apps(down).length) {
		await mail(env, subject(apps(down), "is unreachable", "are unreachable"),
			lines(apps(down), (e) => e.why) + "\n\n" + advice(watchesPanel));
	}
	if (apps(still).length) {
		await mail(env, subject(apps(still), "is still unreachable", "are still unreachable"),
			lines(apps(still), (e) => `${e.why}, for ${minutes(now - e.was.since)}`) + "\n\n" + advice(watchesPanel));
	}
	for (const e of back.filter((e) => e.t.name === "panel")) {
		await mail(env, "The panel answers again", answersAgain(e, now));
	}
	if (apps(back).length) {
		await mail(env, subject(apps(back), "answers again", "answer again"),
			apps(back).map((e) => answersAgain(e, now)).join("\n"));
	}
	if (JSON.stringify(state) !== before) {
		if (Object.keys(state).length) await env.STATE.put("down", JSON.stringify(state));
		else await env.STATE.delete("down");
	}
}

// due are the targets checked this minute: all of them, or with more than
// PER_RUN, the panel and the apps whose turn it is.
export function due(targets, now) {
	if (targets.length <= PER_RUN) return targets;
	const panel = targets[0].name === "panel" ? [targets[0]] : [];
	const apps = targets.slice(panel.length);
	const per = PER_RUN - panel.length;
	const turns = Math.ceil(apps.length / per);
	const turn = Math.floor(now / 60_000) % turns;
	return [...panel, ...apps.slice(turn * per, turn * per + per)];
}

function subject(list, one, many) {
	if (list.length === 1) return `${label(list[0].t)} ${one}`;
	const names = list.map((e) => label(e.t));
	return `${list.length} apps ${many}: ${names.slice(0, 3).join(", ")}${names.length > 3 ? ", …" : ""}`;
}

function lines(list, what) {
	return list.map((e) => `${label(e.t)} at ${e.t.url}${e.t.server ? ` (server ${e.t.server})` : ""}: ${what(e)}.`).join("\n");
}

function answersAgain(e, now) {
	return `${label(e.t)} at ${e.t.url} answers again; it was unreachable from ${new Date(e.was.since).toUTCString()} for ${minutes(now - e.was.since)}.`;
}

function label(t) {
	return t.label || (t.name === "panel" ? "The panel" : t.name.replace(/^app:/, ""));
}

// probeTries asks a target up to TRIES times while it fails.
async function probeTries(t, sleep) {
	let res;
	for (let i = 0; i < TRIES; i++) {
		res = await probe(t);
		if (!res.why) break;
		if (i < TRIES - 1) await sleep(PAUSE_MS);
	}
	return res;
}

// probe returns how long the target took, its status and why it isn't
// healthy, "" when it is.
export async function probe(t) {
	const start = Date.now();
	try {
		const r = await fetch(t.url, {
			redirect: "manual",
			signal: AbortSignal.timeout(TIMEOUT_MS),
			// A trace marked not sampled keeps the checks out of the app's traces.
			headers: { "user-agent": "hakobu-watchdog", "sentry-trace": `${hex(16)}-${hex(8)}-0` },
		});
		const body = t.need === "ok" ? (await r.text()).trim() : (await r.body?.cancel(), "");
		const ms = Date.now() - start;
		const healthy =
			t.need === "ok" ? r.status === 200 && body === "ok" :
			t.need === "2xx" ? r.status >= 200 && r.status < 300 :
			r.status < 500;
		return { ms, status: r.status, why: healthy ? "" : `it answered HTTP ${r.status}${hint(r.status)}` };
	} catch (e) {
		const why = e && e.name === "TimeoutError" ? `no answer within ${TIMEOUT_MS / 1000} s` : `no answer (${e})`;
		return { ms: Date.now() - start, status: 0, why };
	}
}

function hex(bytes) {
	return [...crypto.getRandomValues(new Uint8Array(bytes))].map((b) => b.toString(16).padStart(2, "0")).join("");
}

async function record(db, targets, results, now) {
	const ts = Math.floor(now / 60_000) * 60;
	const insert = db.prepare("INSERT OR REPLACE INTO checks (target, ts, up, ms, status) VALUES (?, ?, ?, ?, ?)");
	const statements = targets.map((t, i) => insert.bind(t.name, ts, results[i].why ? 0 : 1, results[i].ms, results[i].status));
	if (new Date(now).getUTCMinutes() === 0) {
		statements.push(db.prepare("DELETE FROM checks WHERE ts < ?").bind(Math.floor((now - KEEP_MS) / 1000)));
	}
	try {
		await db.batch(statements);
	} catch (e) {
		console.log("recording the checks failed:", e);
	}
}

function hint(status) {
	switch (status) {
		case 502:
			return ": the tunnel is up but can't reach hakobu, which isn't running";
		case 530:
			return ": the tunnel isn't connected, so the server or its cloudflared is down";
		default:
			return "";
	}
}

function panelUnreachable(t, why) {
	return `hakobu's watchdog on Cloudflare can't reach ${t.url.replace(/\/healthz$/, "")}: ${why}. ` +
		"Its apps are likely down too, and hakobu can't send its own emails until it's back. " +
		"Check that the server is running, then `systemctl status hakobu` on it.";
}

function advice(watchesPanel) {
	if (watchesPanel) {
		return "hakobu's watchdog on Cloudflare can't reach them, while the panel answers. " +
			"If hakobu shows an app running, its address is what's broken: its DNS record, the tunnel's route, " +
			"or a Cloudflare rule in front of it.";
	}
	return "hakobu's watchdog on Cloudflare can't reach them. " +
		"An answer 530 means the tunnel isn't connected: the server or its cloudflared is down, " +
		"and hakobu emails too once the server is away from the panel for a few minutes. " +
		"If hakobu shows an app running, its address is what's broken: its DNS record, the tunnel's route, " +
		"or a Cloudflare rule in front of it.";
}

function minutes(ms) {
	const m = Math.round(ms / 60_000);
	return m < 60 ? `${m} min` : `${Math.floor(m / 60)} h ${m % 60} min`;
}

async function mail(env, subject, text) {
	const panel = env.PANEL || JSON.parse(env.TARGETS)[0].url.replace(/\/healthz$/, "");
	text += "\n\n-- \nhakobu's watchdog, a Worker in your Cloudflare account. Settings: " +
		panel + "/settings#notifications\n";
	try {
		await env.EMAIL.send({ from: { email: env.FROM, name: "Hakobu" }, to: env.TO, subject, text });
	} catch (e) {
		// Bindings without Email Sending take a raw message.
		const { EmailMessage } = await import("cloudflare:email");
		const raw = [
			`From: Hakobu <${env.FROM}>`,
			`To: ${env.TO}`,
			`Subject: ${subject}`,
			`Message-ID: <${crypto.randomUUID()}@${env.FROM.split("@")[1]}>`,
			`Date: ${new Date().toUTCString()}`,
			"MIME-Version: 1.0",
			"Content-Type: text/plain; charset=utf-8",
			"",
			text,
		].join("\r\n");
		await env.EMAIL.send(new EmailMessage(env.FROM, env.TO, raw));
	}
}
