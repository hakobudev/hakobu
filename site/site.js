// hakobu.dev: the live panel under the hero, the Rollback tile, copy buttons.

const $ = (s, el = document) => el.querySelector(s);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const still = matchMedia("(prefers-reduced-motion: reduce)").matches;
const NS = "http://www.w3.org/2000/svg";

// The live panel: lines between the canvas's cards, as the panel draws
// them, and one push going out on a loop.
const demo = $("#demo"), canvas = $("#canvas"), svg = $("#canvas-lines");
const LINKS = [["app-chat", "app-api", "call"], ["app-admin", "app-api", "call"], ["app-api", "db-app"], ["app-api", "cache-queue"], ["app-api", "st-files"]];
const paths = {};

function anchor(el, side, box) {
  const r = el.getBoundingClientRect();
  return [(side === "r" ? r.right : r.left) - box.left, r.top + Math.min(r.height / 2, 30) - box.top];
}

function drawLines() {
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
    paths[from + ">" + to] = path;
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

// A dot carried along a line.
function carry(key, ms) {
  const path = paths[key];
  if (!path) return sleep(ms);
  path.classList.add("is-hot");
  const len = path.getTotalLength(), dot = document.createElementNS(NS, "circle");
  dot.setAttribute("class", "packet");
  dot.setAttribute("r", 4.5);
  svg.append(dot);
  return new Promise(done => {
    const t0 = performance.now();
    (function step(now) {
      const t = Math.min(1, (now - t0) / ms), e = t < .5 ? 2 * t * t : 1 - (-2 * t + 2) ** 2 / 2;
      const p = path.getPointAtLength(e * len);
      dot.setAttribute("cx", p.x);
      dot.setAttribute("cy", p.y);
      if (t < 1) return requestAnimationFrame(step);
      dot.remove();
      done();
    })(t0);
  });
}

const logLines = [...document.querySelectorAll("#log-body div")];
const ICON = name => `<svg class="icon icon-sm${name === "loader-circle" ? " spin" : " text-success"}"><use href="#i-${name}"/></svg>`;

function api(state, when, busy) {
  $("#api-state").innerHTML = ICON(busy ? "loader-circle" : "circle-check") + `<span>${state}</span>`;
  $("#api-when").textContent = when;
  $("#api-dot").className = "dot " + (busy ? "dot-busy" : "dot-success");
}

function log(state, dot) {
  $("#log-state").textContent = state;
  $("#log-dot").className = "dot " + dot;
}

const hot = (sel, on) => $(sel).classList.toggle("is-hot", on);

function reset() {
  $("#push-out").style.visibility = "hidden";
  $("#caret").style.display = "";
  $("#push").classList.remove("is-away");
  $("#log").classList.add("is-away");
  logLines.forEach(l => l.classList.remove("is-on"));
  Object.values(paths).forEach(p => p.classList.remove("is-hot"));
  ["#app-api", "#app-chat", "#db-app", "#dom-chat"].forEach(s => hot(s, false));
  api("Deployed 7d20e4a", "yesterday", false);
  log("Queued", "");
}

function finish() {
  $("#push-out").style.visibility = "visible";
  $("#caret").style.display = "none";
  logLines.forEach(l => l.classList.add("is-on"));
  api("Deployed 9e41b7c", "just now", false);
  log("Live", "dot-success");
}

async function play() {
  for (;;) {
    reset();
    await sleep(1400);
    $("#caret").style.display = "none";
    $("#push-out").style.visibility = "visible";
    await sleep(700);
    $("#log").classList.remove("is-away");
    log("Building", "dot-busy");
    hot("#app-api", true);
    api("Building 9e41b7c", "now", true);
    for (const l of logLines.slice(0, 2)) {
      await sleep(700);
      l.classList.add("is-on");
    }
    await sleep(500);
    api("Snapshotting app-db", "now", true);
    hot("#db-app", true);
    await carry("app-api>db-app", 900);
    logLines[2].classList.add("is-on");
    hot("#db-app", false);
    await sleep(500);
    api("Starting green", "now", true);
    logLines[3].classList.add("is-on");
    await sleep(1100);
    logLines[4].classList.add("is-on");
    api("Deployed 9e41b7c", "just now", false);
    log("Live", "dot-success");
    // The first request after the switch: in at the frontend's address,
    // on to the API.
    hot("#dom-chat", true);
    hot("#app-chat", true);
    await sleep(400);
    await carry("app-chat>app-api", 750);
    hot("#app-api", true);
    await sleep(900);
    Object.values(paths).forEach(p => p.classList.remove("is-hot"));
    ["#app-api", "#app-chat", "#dom-chat"].forEach(s => hot(s, false));
    await sleep(3200);
    $("#push").classList.add("is-away");
    $("#log").classList.add("is-away");
    await sleep(700);
  }
}

drawLines();
addEventListener("resize", drawLines);
document.fonts?.ready.then(drawLines);
if (still) {
  demo.classList.add("is-static");
  finish();
} else {
  play();
}

// Rollback: each click goes one deploy back, undoing the newer ones.
const deploys = [...document.querySelectorAll("#deploys .deploy")];
let live = 0;
$("#rollback").addEventListener("click", () => {
  live = live === deploys.length - 1 ? 0 : live + 1;
  deploys.forEach((d, i) => {
    d.classList.toggle("is-live", i === live);
    d.classList.toggle("is-undone", i < live);
  });
  $("#rollback-label").textContent = live === deploys.length - 1 ? "Start over" : "Roll back";
});

document.querySelectorAll("[data-copy]").forEach(b => b.addEventListener("click", () => {
  navigator.clipboard.writeText(b.dataset.copy).then(() => {
    b.classList.add("is-copied");
    setTimeout(() => b.classList.remove("is-copied"), 1500);
  });
}));
