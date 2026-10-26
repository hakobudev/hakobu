// hakobu.dev: the repository under the hero (its files, and a push played
// in the terminal next to them), the Rollback tile, copy buttons.

const $ = (s, el = document) => el.querySelector(s);
const still = matchMedia("(prefers-reduced-motion: reduce)").matches;
const wait = ms => new Promise(r => setTimeout(r, still ? 0 : ms));

// The files: one tab at a time.
const tabs = document.querySelectorAll(".file-tab");
tabs.forEach(t => t.addEventListener("click", () => {
  tabs.forEach(o => {
    o.classList.toggle("is-current", o === t);
    o.setAttribute("aria-selected", o === t);
  });
  document.querySelectorAll(".code[data-file]").forEach(c => { c.hidden = c.dataset.file !== t.dataset.file; });
}));

// A push, as CI, the server and the commit see it: [source, step, detail,
// class of the detail]. A source of "" is the shell; "nova-1" the server.
const PUSHES = {
  ok: {
    sha: "a1f3c9e", msg: "Add invoices",
    lines: [
      ["", "git push", "main → a1f3c9e Add invoices"],
      ["CI", "validate", "hakobu.toml ok"],
      ["CI", "build", "web → ghcr.io/nova/crm-web@sha256:90c7…"],
      ["CI", "build", "admin → ghcr.io/nova/crm-admin@sha256:4be1…"],
      ["CI", "release", "ghcr.io/nova/crm:main, last"],
      ["CI", "wait", "for hakobu/deploy…", "", "pending"],
      ["nova-1", "found", "a new release, a1f3c9e"],
      ["nova-1", "plan", "2 apps, 1 database, 1 cache"],
      ["nova-1", "dump", "main, before the migration"],
      ["nova-1", "release", "bin/rails db:migrate  ok in 3.8s", "ok"],
      ["nova-1", "start", "web, admin: GET /up 200", "ok"],
      ["nova-1", "switch", "live; the old version stopped", "ok"],
      ["CI", "status", "hakobu/deploy success: job green", "ok", "success"],
    ],
  },
  bad: {
    sha: "b72e01d", msg: "Rename invoices.total",
    lines: [
      ["", "git push", "main → b72e01d Rename invoices.total"],
      ["CI", "validate", "hakobu.toml ok"],
      ["CI", "build", "web → ghcr.io/nova/crm-web@sha256:e3a0…"],
      ["CI", "release", "ghcr.io/nova/crm:main, last"],
      ["CI", "wait", "for hakobu/deploy…", "", "pending"],
      ["nova-1", "found", "a new release, b72e01d"],
      ["nova-1", "plan", "1 app changed"],
      ["nova-1", "dump", "main, before the migration"],
      ["nova-1", "release", "bin/rails db:migrate  FAILED", "err"],
      ["nova-1", "", "PG::UndefinedColumn: column \"total\" does not exist", "err"],
      ["nova-1", "kept", "the running version; nothing changed", "ok"],
      ["mail", "sent", "[hakobu nova-1] deploy of crm failed: release …"],
      ["CI", "status", "hakobu/deploy failure: job red", "err", "failure"],
    ],
  },
};

const term = $("#term"), status = $("#commit-status");
const pushButtons = document.querySelectorAll("[data-push]");
let playing = false;

function line([src, step, det, cls = ""], fresh) {
  const ln = document.createElement("div");
  ln.className = "ln" + (fresh ? " is-new" : "");
  if (src === "") {
    ln.innerHTML = `<span><span class="p">$</span> ${step} <span class="m">${det}</span></span>`;
  } else {
    const s = document.createElement("span");
    s.className = "src" + (src === "nova-1" ? " is-srv" : src === "mail" ? " is-mail" : "");
    s.textContent = src;
    const st = document.createElement("span");
    st.className = "step";
    st.textContent = step;
    const d = document.createElement("span");
    d.className = "det" + (cls ? " " + cls : "");
    d.textContent = det;
    ln.append(s, st, d);
  }
  return ln;
}

async function push(kind) {
  if (playing) return;
  playing = true;
  pushButtons.forEach(b => { b.disabled = true; });
  const p = PUSHES[kind];
  $("#commit-sha").textContent = p.sha;
  $("#commit-msg").textContent = p.msg;
  status.dataset.state = "none";
  term.replaceChildren();
  for (const l of p.lines) {
    term.append(line(l, true));
    if (l[4]) status.dataset.state = l[4];
    term.scrollTop = term.scrollHeight;
    await wait(l[0] === "nova-1" ? 520 : 380);
  }
  playing = false;
  pushButtons.forEach(b => { b.disabled = false; });
}

pushButtons.forEach(b => b.addEventListener("click", () => push(b.dataset.push)));

// Until one is picked, the last push, as it went.
term.append(...PUSHES.ok.lines.map(l => line(l, false)));

// Rollback: each click goes one release back and holds the newer ones,
// as hakobu rollback does; at the oldest, start over.
const deploys = [...document.querySelectorAll("#deploys .deploy")];
let live = 0;
$("#rollback")?.addEventListener("click", () => {
  live = live === deploys.length - 1 ? 0 : live + 1;
  deploys.forEach((d, i) => {
    d.classList.toggle("is-live", i === live);
    d.classList.toggle("is-held", i < live);
  });
  $("#rollback-label").textContent = live === deploys.length - 1 ? "start over" : "ssh nova-1 rollback crm";
});

document.querySelectorAll("[data-copy]").forEach(b => b.addEventListener("click", () => {
  navigator.clipboard.writeText(b.dataset.copy).then(() => {
    b.classList.add("is-copied");
    setTimeout(() => b.classList.remove("is-copied"), 1500);
  });
}));
