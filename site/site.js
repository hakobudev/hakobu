// hakobu.dev: the demo panel under the hero (the panel's pages with
// made-up data, to click through), the Rollback tile, copy buttons.

const $ = (s, el = document) => el.querySelector(s);
const NS = "http://www.w3.org/2000/svg";
const icon = (name, cls = "") => `<svg class="icon ${cls}"><use href="#i-${name}"/></svg>`;
const logo = name => `<svg class="logo"><use href="#l-${name}"/></svg>`;

// Lines between the canvas's cards, as the panel draws them.
const LINKS = [["app-chat", "app-api", "call"], ["app-admin", "app-api", "call"], ["app-api", "db-app"], ["app-api", "cache-queue"], ["app-api", "st-files"]];

function anchor(el, side, box) {
  const r = el.getBoundingClientRect();
  return [(side === "r" ? r.right : r.left) - box.left, r.top + Math.min(r.height / 2, 30) - box.top];
}

function drawLines() {
  const canvas = $("#canvas"), svg = $("#canvas-lines");
  if (!canvas) return; // another page of the demo is open
  svg.replaceChildren();
  const box = canvas.getBoundingClientRect();
  for (const [from, to, kind] of LINKS) {
    const a = $("#" + from), b = $("#" + to);
    if (!a.offsetParent || !b.offsetParent) continue; // a column hidden on narrow screens
    const ra = a.getBoundingClientRect(), rb = b.getBoundingClientRect();
    let p1, p2, d;
    if (Math.abs(ra.left - rb.left) < 32) { // one column: around its left side, as the panel draws it
      p1 = anchor(a, "l", box), p2 = anchor(b, "l", box);
      const out = p1[0] - 28;
      d = `M${p1} C${out},${p1[1]} ${out},${p2[1]} ${p2}`;
    } else {
      p1 = anchor(a, "r", box), p2 = anchor(b, "l", box);
      const m = (p1[0] + p2[0]) / 2;
      d = `M${p1} C${m},${p1[1]} ${m},${p2[1]} ${p2}`;
    }
    const path = document.createElementNS(NS, "path");
    path.setAttribute("d", d);
    if (kind) path.setAttribute("class", "is-call");
    svg.append(path);
    for (const [x, y] of [p1, p2]) {
      const c = document.createElementNS(NS, "circle");
      c.setAttribute("class", "end");
      c.setAttribute("cx", x);
      c.setAttribute("cy", y);
      c.setAttribute("r", 3);
      svg.append(c);
    }
  }
}

drawLines();
addEventListener("resize", drawLines);
document.fonts?.ready.then(drawLines);

// The demo's made-up project.
const APPS = {
  chat: {
    logo: "nextdotjs", build: "Railpack", repo: "acme/chat", address: "chat.acme.dev", linked: ["app-db", "files"], uptime: [100, 99.98, 99.95, 86],
    deploys: [
      { sha: "a1f3c9e", msg: "Stream replies into the page", when: "2h ago", status: "live" },
      { sha: "60be2d1", msg: "Typing indicator", when: "yesterday", status: "ok" },
      { sha: "f4410ac", msg: "Upgrade Next.js", when: "3 days ago", status: "ok" },
    ],
    problems: [],
    logs: [["12:04:11", "info", "GET /c/4821 200 38ms"], ["12:04:12", "info", "POST /api/send 200 112ms"], ["12:04:15", "info", "GET /c/4821/stream 200 4.2s"], ["12:04:19", "info", "GET / 200 21ms"]],
    routes: [["/c/[id]", 9120, 34, 120, "0.0"], ["/api/send", 4410, 98, 310, "0.1"], ["/", 2980, 18, 44, "0.0"]],
    vars: [["API_URL", "${{api.PRIVATE_URL}}"], ["DATABASE_URL", "from app-db", true], ["NEXT_PUBLIC_SITE", "https://chat.acme.dev"], ["SESSION_SECRET", null]],
  },
  admin: {
    logo: "django", build: "Dockerfile", repo: "acme/admin", address: "admin.acme.dev", linked: ["app-db"], uptime: [100, 100, 99.99, 64],
    deploys: [
      { sha: "5b0e7d2", msg: "Export orders as CSV", when: "3 days ago", status: "live" },
      { sha: "c90a4f7", msg: "Staff can refund", when: "last week", status: "ok" },
    ],
    problems: [{ title: "DisallowedHost: 'admin.acme.dev:443'", kind: "error", when: "2 days ago", count: 3, stack: ["django/http/request.py:151 in get_host", "acme/middleware.py:22 in __call__"] }],
    logs: [["11:58:02", "info", "GET /admin/orders/ 200 64ms"], ["11:58:40", "info", "GET /admin/orders/export.csv 200 812ms"], ["12:01:05", "warning", "Slow query: 1.4 s on orders"]],
    routes: [["/admin/orders/", 820, 52, 210, "0.0"], ["/admin/orders/export.csv", 41, 640, 1900, "0.0"]],
    vars: [["DATABASE_URL", "from app-db", true], ["DJANGO_SECRET_KEY", null], ["ALLOWED_HOSTS", "admin.acme.dev"]],
  },
  api: {
    logo: "fastapi", build: "Railpack", repo: "acme/api", private: "http://api.hakobu:8000", linked: ["app-db", "queue", "files"], worker: "arq jobs.WorkerSettings",
    deploys: [
      { sha: "9e41b7c", msg: "Stream replies", when: "4 min ago", status: "live" },
      { sha: "7d20e4a", msg: "Rename messages.body", when: "yesterday", status: "ok" },
      { sha: "3c1a0f2", msg: "Retry Redis on timeout", when: "yesterday", status: "failed", log: ["Cloning acme/api at 3c1a0f2", "Building with Railpack", "Snapshotting app-db before the deploy", "Starting the new version next to the old one", "GET /health: no answer in 60 s"], note: "Failed its health check; the old version kept serving" },
      { sha: "d82f6b1", msg: "File uploads", when: "4 days ago", status: "ok" },
    ],
    problems: [
      { title: "KeyError: 'body'", kind: "error", when: "9 min ago", count: 23, stack: ["chat/stream.py:42 in reply", "routes/messages.py:17 in post"] },
      { title: "TimeoutError: Redis command timed out", kind: "error", when: "5h ago", count: 2, stack: ["arq/connections.py:211 in enqueue_job", "jobs/notify.py:9 in schedule"] },
      { title: "Worker restarted after running out of memory", kind: "oom", when: "3 days ago", count: 1, stack: ["512 MB limit reached by jobs.resize_upload"] },
    ],
    logs: [["12:04:11", "info", "POST /messages 201 24ms"], ["12:04:12", "info", "GET /chat/stream 200 3.9s"], ["12:04:13", "error", "KeyError: 'body' in chat/stream.py:42"], ["12:04:14", "info", "worker: job notify:8812 done in 120ms"], ["12:04:18", "info", "GET /health 200 2ms"]],
    routes: [["/chat/stream", 18240, 41, 310, "0.2"], ["/messages/{id}", 9310, 12, 60, "0.0"], ["/auth/session", 4022, 6, 18, "0.0"]],
    vars: [["DATABASE_URL", "from app-db", true], ["REDIS_URL", "from queue", true], ["S3_BUCKET", "from files", true], ["OPENAI_API_KEY", null], ["LOG_LEVEL", "info"]],
  },
};
const BUILD_LOG = ["Cloning {repo} at {sha}", "Building with {build}", "Snapshotting app-db before the deploy", "Starting the new version next to the old one", "GET /health answered 200 in 41 ms", "Switched traffic; the old version stopped"];
const KIND = { "app-db": "db", queue: "cache", files: "storage", chat: "app", api: "app", admin: "app" };
const ICON = { "app-db": "database", queue: "zap", files: "archive", chat: "box", api: "box", admin: "box" };

const win = $("#win"), body = $("#win-body"), crumbs = $("#crumbs");
const projectHTML = body.innerHTML, crumbsHTML = crumbs.innerHTML;
let view = { kind: "project", tab: "overview" }, open = null, restoring = false, busy = false;

const box = (title, inner, aside = "") => `<div class="box"><div class="box-head"><span>${title}</span>${aside}</div>${inner}</div>`;
const chips = names => names.map(n => `<button type="button" class="chip-link" data-open="${KIND[n]}:${n}">${icon(ICON[n])}${n}</button>`).join("");
const kv = rows => `<div class="kv">${rows.map(([k, v]) => `<div class="kv-k">${k}</div><div class="kv-v">${v}</div>`).join("")}</div>`;
const sealed = `<span class="sealed">${icon("lock")}sealed</span>`;
const varRows = list => list.map(([k, v, from]) => `<div class="list-row"><span class="mono strong var-k">${k}</span><span class="flex-1 truncate ${from ? "muted" : "mono"}">${v === null ? sealed : v}</span></div>`).join("");

function header(iconHTML, title, badge, meta, actions = "") {
  return `<div class="page-header"><div class="app-title"><span class="icon-tile">${iconHTML}</span><div>
    <div class="page-title">${title}${badge ? ` <span class="badge"><span class="dot dot-success"></span>${badge}</span>` : ""}</div>
    <div class="meta">${meta.map(([i, t]) => `<span class="meta-item">${icon(i)}${t}</span>`).join("")}</div></div></div>
    ${actions ? `<div class="header-actions">${actions}</div>` : ""}</div>`;
}

function tabs(list, current, attr) {
  return `<div class="tabs" role="tablist">${list.map(([id, i, label]) =>
    `<button type="button" role="tab" class="tab${id === current ? " is-current" : ""}" aria-selected="${id === current}" ${attr}="${id}">${icon(i)}${label}</button>`).join("")}</div>`;
}

function uptimeBox(u) {
  const days = Array.from({ length: 30 }, (_, i) => `<i${i === 17 ? ' class="is-down"' : ""}></i>`).join("");
  return box("From outside", `<div class="stats">${[["24 hours", u[0], "%"], ["7 days", u[1], "%"], ["30 days", u[2], "%"], ["Answers in", u[3], "ms"]]
    .map(([l, v, unit]) => `<div class="stat"><span class="muted">${l}</span><b>${unit === "%" ? v.toFixed(2) : v}<small>${unit}</small></b></div>`).join("")}</div>
    <div class="days" aria-hidden="true">${days}</div><div class="days-legend"><span>30 days ago</span><span>One outage, 4 min</span><span>Today</span></div>`,
    `<span class="muted">checked every minute</span>`);
}

function problemRows(list) {
  if (!list.length) return `<div class="empty">${icon("circle-check", "text-success")}No problems in the last 7 days.</div>`;
  return list.map((p, i) => `<button type="button" class="list-row list-row-2 row-btn" data-act="open" data-i="${i}">
    ${icon(p.kind === "error" ? "triangle-alert" : "activity", p.kind === "error" ? "text-danger" : "icon-muted")}
    <span class="min-w-0 flex-1"><span class="strong truncate block">${p.title}</span><span class="muted small block">${p.kind} · ${p.when}${p.count > 1 ? ` · ${p.count} events` : ""}</span>
    ${open === i ? `<span class="stack mono block">${p.stack.map(l => `<span class="block">${l}</span>`).join("")}</span>` : ""}</span></button>`).join("");
}

function deployRows(a) {
  return a.deploys.map((d, i) => {
    const st = d.status === "building" ? icon("loader-circle", "spin") : d.status === "failed" ? icon("circle-x", "text-danger") : icon("circle-check", "text-success");
    const tag = d.status === "live" ? `<span class="live-tag">Live</span>` : d.status === "building" ? `<span class="muted small">Building</span>` : d.status === "failed" ? `<span class="muted small">Failed</span>` : "";
    const lines = d.log || BUILD_LOG.map(l => l.replace("{sha}", d.sha).replace("{repo}", a.repo).replace("{build}", a.build));
    const log = open === i ? `<span class="deploy-log mono block">${lines.map(l => `<span class="block">${l}</span>`).join("")}${d.note ? `<span class="note block">${d.note}</span>` : ""}</span>` : "";
    return `<button type="button" class="list-row list-row-2 row-btn" data-act="open" data-i="${i}">${st}<span class="min-w-0 flex-1">
      <span class="deploy-line"><span class="mono muted">${d.sha}</span><span class="truncate flex-1">${d.msg}</span>${tag}<span class="muted small">${d.when}</span></span>${log}</span></button>`;
  }).join("");
}

function appPage(name, tab) {
  const a = APPS[name];
  let h = header(logo(a.logo), name, "Running", [["github", `<span class="mono">${a.repo}</span>`], ["box", a.build], ["globe", a.address || "private"]],
    `<button type="button" class="btn" data-act="rollback">${icon("history")}Rollback</button><button type="button" class="btn btn-primary" data-act="redeploy">${icon("refresh")}Redeploy</button>`);
  h += tabs([["overview", "activity", "Overview"], ["deployments", "history", "Deployments"], ["logs", "terminal", "Logs"], ["errors", "triangle-alert", "Errors"], ["performance", "gauge", "Performance"], ["variables", "key-round", "Variables"]], tab, "data-tab");
  if (tab === "overview") {
    const live = a.deploys.find(d => d.status === "live");
    const main = box("Production", kv([
      ["Address", a.address ? `<span class="link">${a.address}</span>` : `<span class="mono">${a.private}</span> <span class="muted small">only this project's apps reach it</span>`],
      ["Build", a.build], ["Linked", `<div class="chip-row">${chips(a.linked)}</div>`]]),
      `<span class="muted">${icon("circle-check", "text-success")} Deployed ${live.sha} · ${live.when}</span>`)
      + (a.uptime ? uptimeBox(a.uptime) : "");
    const side = (a.worker ? box("Worker", `<div class="box-pad"><div class="mono">${a.worker}</div><p class="muted small">Same image as the app; restarts after every deploy.</p></div>`, `<span class="muted status"><span class="dot dot-success"></span>running</span>`) : "")
      + box("Latest problems", problemRows(a.problems.slice(0, 3)), `<button type="button" class="link link-btn" data-tab="errors">View all</button>`);
    return h + `<div class="app-grid"><div class="app-col">${main}</div><div class="app-col">${side}</div></div>`;
  }
  if (tab === "deployments") return h + box("Deployments", deployRows(a), `<span class="muted">click one for its log</span>`);
  if (tab === "logs") return h + `<div class="logbox mono">${a.logs.map(([t, l, m]) => `<div class="log-${l}"><span class="muted">${t}</span> <span class="lvl">${l}</span> ${m}</div>`).join("")}</div>`;
  if (tab === "errors") return h + `<p class="muted small tab-note">Errors and logs sent by any Sentry SDK using the SENTRY_DSN hakobu sets, plus crashes, out-of-memory restarts and failed health checks. Click one for its stack.</p>` + box("Problems", problemRows(a.problems));
  if (tab === "performance") return h + `<div class="box"><table class="routes"><thead><tr><th>Route</th><th>Requests</th><th>p50</th><th>p95</th><th>Failed</th></tr></thead><tbody>${a.routes.map(([r, n, p50, p95, f]) =>
    `<tr><td class="mono">${r}</td><td>${n.toLocaleString("en")}</td><td>${p50} ms</td><td>${p95} ms</td><td>${f}%</td></tr>`).join("")}</tbody></table></div>`;
  return h + box("Variables", varRows(a.vars), `<span class="muted">sealed values are never shown again</span>`);
}

function dbPage() {
  const backups = [["3h ago", "132 MB"], ["yesterday", "131 MB"], ["2 days ago", "129 MB"], ["3 days ago", "129 MB"]];
  return header(logo("postgresql"), "app-db", "Ready", [["database", "PostgreSQL"], ["key-round", `user <span class="mono">db_1</span>`]])
    + `<div class="app-grid"><div class="app-col">`
    + box("Backups", `<p class="box-pad muted small">Daily to R2 in your Cloudflare account, sealed with the server's key. Each one is restored into a scratch database right after upload.</p>`
      + backups.map(([w, s], i) => `<div class="list-row">${icon("circle-check", "text-success")}<span class="flex-1">${w} · ${s} <span class="tag">restores · 21 tables</span></span>${open === i
        ? `<span class="muted small">${restoring ? "Restoring…" : "Restored, 21 tables"}</span>` : `<button type="button" class="btn btn-sm" data-act="restore" data-i="${i}">Restore</button>`}</div>`).join(""))
    + `</div><div class="app-col">`
    + box("Connection", kv([["Host", `<span class="mono">hakobu-postgres</span>`], ["Reached by", "this project's apps only"], ["Used by", `<div class="chip-row">${chips(["chat", "api", "admin"])}</div>`]]))
    + `</div></div>`;
}

function cachePage() {
  return header(icon("zap"), "queue", "Running", [["zap", "Valkey, Redis-compatible"], ["server", "256 MB of memory"]])
    + `<div class="app-grid"><div class="app-col">`
    + box("Memory", `<div class="box-pad"><div class="meter"><i style="width:47%"></i></div><p class="muted small">120 MB of 256 MB, 18,204 keys. When full, the least recently used keys go first.</p></div>`)
    + box("Backups", `<div class="empty">${icon("history", "icon-muted")}Not backed up: a cache holds what your apps can rebuild.</div>`)
    + `</div><div class="app-col">` + box("Used by", `<div class="box-pad"><div class="chip-row">${chips(["api"])}</div><p class="muted small">It gets <span class="mono">REDIS_URL</span>.</p></div>`) + `</div></div>`;
}

function storagePage() {
  return header(logo("cloudflare"), "files", "", [["archive", "Cloudflare R2"], ["globe", `<span class="mono">acme-files-3f9a1c</span>`]])
    + `<div class="app-grid"><div class="app-col">`
    + box("Bucket", kv([["Objects", "5,646"], ["Size", "566 MB"], ["Keys", "set; they reach this bucket only"]]))
    + `</div><div class="app-col">` + box("Used by", `<div class="box-pad"><div class="chip-row">${chips(["chat", "api"])}</div><p class="muted small">They get <span class="mono">S3_ENDPOINT</span>, <span class="mono">S3_BUCKET</span> and the keys.</p></div>`) + `</div></div>`;
}

function projectTab(tab) {
  const tmp = document.createElement("div");
  tmp.innerHTML = projectHTML;
  const h = tmp.querySelector(".page-header").outerHTML
    + tabs([["overview", "layers", "Overview"], ["variables", "key-round", "Shared variables"], ["settings", "settings", "Settings"]], tab, "data-ptab");
  if (tab === "variables") return h + box("Shared by every app in acme", varRows([["SENTRY_ENVIRONMENT", "production"], ["TZ", "UTC"], ["STRIPE_SECRET_KEY", null]]));
  return h + box("Project", kv([["Server", `hetzner-1 <span class="muted small">connected</span>`], ["Cloudflare", `acme <span class="muted small">acme.dev</span>`],
    ["Backups", `<span class="mono">hakobu-backups-e2a3e4</span> <span class="muted small">in acme's R2</span>`]]));
}

function render() {
  if (view.kind === "project") {
    crumbs.innerHTML = crumbsHTML;
    body.innerHTML = view.tab === "overview" ? projectHTML : projectTab(view.tab);
    drawLines();
    return;
  }
  const name = view.name;
  const ic = view.kind === "app" ? logo(APPS[name].logo) : view.kind === "db" ? logo("postgresql") : view.kind === "storage" ? logo("cloudflare") : icon("zap");
  crumbs.innerHTML = `<span class="wordmark">hakobu</span><span class="context-sep">/</span><button type="button" class="crumb" data-go="project">acme</button><span class="context-sep">/</span><span class="context-item">${ic}${name}</span>`;
  body.innerHTML = view.kind === "app" ? appPage(name, view.tab) : view.kind === "db" ? dbPage() : view.kind === "cache" ? cachePage() : storagePage();
}

function go(v) {
  view = v;
  open = null;
  render();
}

const SHAS = ["b81c0d4", "e2f7a91", "5a0c3be", "0d9e6f3"];

async function redeploy(name) {
  if (busy) return;
  busy = true;
  const a = APPS[name], prev = a.deploys.find(d => d.status === "live");
  const d = { sha: SHAS.shift() || "1f2e3d4", msg: `Redeploy of ${prev.sha}`, when: "now", status: "building", log: [] };
  a.deploys.unshift(d);
  go({ kind: "app", name, tab: "deployments" });
  open = 0;
  for (const line of BUILD_LOG) {
    d.log.push(line.replace("{sha}", d.sha).replace("{repo}", a.repo).replace("{build}", a.build));
    if (view.name === name && view.tab === "deployments") render();
    await new Promise(r => setTimeout(r, 650));
  }
  d.status = "live";
  d.when = "just now";
  prev.status = "ok";
  busy = false;
  if (view.name === name) render();
}

function rollback(name) {
  const a = APPS[name], i = a.deploys.findIndex(d => d.status === "live");
  const back = a.deploys.findIndex((d, j) => j > i && d.status === "ok");
  if (busy || back < 0) return;
  a.deploys[i].status = "ok";
  a.deploys[back].status = "live";
  a.deploys[back].when = "rolled back just now";
  go({ kind: "app", name, tab: "deployments" });
}

win.addEventListener("click", e => {
  const t = e.target.closest("[data-open],[data-ptab],[data-tab],[data-go],[data-act]");
  if (!t) return;
  if (t.dataset.open) {
    const [kind, name] = t.dataset.open.split(":");
    return go({ kind, name, tab: "overview" });
  }
  if (t.dataset.ptab) return go({ kind: "project", tab: t.dataset.ptab });
  if (t.dataset.tab) return go({ ...view, tab: t.dataset.tab });
  if (t.dataset.go) return go({ kind: "project", tab: "overview" });
  const i = Number(t.dataset.i);
  switch (t.dataset.act) {
    case "open":
      open = open === i ? null : i;
      return render();
    case "redeploy": return redeploy(view.name);
    case "rollback": return rollback(view.name);
    case "restore":
      open = i;
      restoring = true;
      render();
      return setTimeout(() => { restoring = false; if (view.kind === "db") render(); }, 1200);
  }
});
win.addEventListener("keydown", e => {
  if ((e.key === "Enter" || e.key === " ") && e.target.matches("[data-open][role=button]")) {
    e.preventDefault();
    e.target.click();
  }
});

// Rollback: each click goes one deploy back, undoing the newer ones.
const deploys = [...document.querySelectorAll("#deploys .deploy")];
let live = 0;
$("#rollback")?.addEventListener("click", () => {
  live = live === deploys.length - 1 ? 0 : live + 1;
  deploys.forEach((d, i) => {
    d.classList.toggle("is-live", i === live);
    d.classList.toggle("is-undone", i < live);
  });
  $("#rollback-label").textContent = live === deploys.length - 1 ? "Start over" : "Roll back";
});

// g, or ?grid in the address, shows the page's columns.
if (new URLSearchParams(location.search).has("grid")) document.body.classList.add("show-grid");
addEventListener("keydown", e => {
  if (e.key === "g" && !e.target.closest("input, textarea, [contenteditable]")) document.body.classList.toggle("show-grid");
});

document.querySelectorAll("[data-copy]").forEach(b => b.addEventListener("click", () => {
  navigator.clipboard.writeText(b.dataset.copy).then(() => {
    b.classList.add("is-copied");
    setTimeout(() => b.classList.remove("is-copied"), 1500);
  });
}));
