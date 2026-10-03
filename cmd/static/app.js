// The panel's own script, served from the binary like everything it loads.
// It's loaded in <head> after htmx, so the theme is set before the page
// paints. Behavior is wired by data-* attributes, set up for the page and
// again for every part htmx loads (htmx.onLoad).

// Theme: the system's unless the owner picked one in Settings.
const themeKey = 'hakobu-theme';
try {
  const saved = localStorage.getItem(themeKey);
  if (saved === 'light' || saved === 'dark') document.documentElement.dataset.theme = saved;
} catch (_) {}

// The choice in Settings: "system" follows the system's.
function setTheme(choice) {
  const root = document.documentElement;
  if (choice === 'light' || choice === 'dark') root.dataset.theme = choice;
  else delete root.dataset.theme;
  try {
    if (root.dataset.theme) localStorage.setItem(themeKey, choice);
    else localStorage.removeItem(themeKey);
  } catch (_) {}
  markTheme(document);
}

function markTheme(root) {
  const current = document.documentElement.dataset.theme || 'system';
  for (const b of root.querySelectorAll('[data-theme-choice]')) b.setAttribute('aria-pressed', String(b.dataset.themeChoice === current));
}

function toast(msg) {
  const el = document.getElementById('toast');
  el.textContent = msg || 'Something went wrong';
  el.hidden = false;
  clearTimeout(el._t);
  el._t = setTimeout(() => el.hidden = true, 8000);
}
document.addEventListener('htmx:responseError', e => toast(e.detail.xhr.responseText));
document.addEventListener('htmx:sendError', () => toast('Network error'));

document.addEventListener('click', e => {
  const t = e.target;
  if (t.id === 'toast') t.hidden = true;
  const theme = t.closest('[data-theme-choice]');
  if (theme) setTheme(theme.dataset.themeChoice);
  // Dialogs: data-dialog opens one by id, data-close or a click on the
  // backdrop closes it.
  const opener = t.closest('[data-dialog]');
  if (opener) {
    opener.closest('[popover]')?.hidePopover();
    document.getElementById(opener.dataset.dialog)?.showModal();
  }
  if (t.closest('[data-close]')) t.closest('dialog')?.close();
  if (t.matches('dialog')) t.close();
  // A menu closes once one of its items is picked.
  if (t.closest('.menu-item')) t.closest('[popover]')?.hidePopover();
  // Values hidden until asked: the row's [data-secret].
  const reveal = t.closest('[data-reveal]');
  if (reveal) {
    const v = reveal.closest('.box-row').querySelector('[data-secret]');
    const shown = v.textContent !== '••••••••';
    v.textContent = shown ? '••••••••' : v.dataset.secret;
    v.classList.toggle('muted', shown);
    reveal.querySelector('use')?.setAttribute('href', shown ? '#i-eye' : '#i-eye-off');
  }
});

// Menus are popovers in the top layer; put each under its button.
document.addEventListener('toggle', e => {
  const menu = e.target;
  if (!menu.matches?.('[popover].menu') || e.newState !== 'open') return;
  const button = document.querySelector(`[popovertarget="${menu.id}"]`);
  if (!button) return;
  const r = button.getBoundingClientRect();
  const start = menu.dataset.align === 'start' ? r.left : r.right - menu.offsetWidth;
  const left = Math.max(8, Math.min(start, innerWidth - menu.offsetWidth - 8));
  menu.style.top = `${r.bottom + 4}px`;
  menu.style.left = `${left}px`;
}, true);

// Copy buttons: <button class="copy" data-copy="text">. The icon turns into
// a check for a moment.
document.addEventListener('click', async e => {
  const btn = e.target.closest('[data-copy]');
  if (!btn) return;
  e.preventDefault();
  try { await navigator.clipboard.writeText(btn.dataset.copy); } catch (_) { return; }
  const use = btn.querySelector('use');
  btn.classList.add('is-copied');
  use?.setAttribute('href', '#i-check');
  clearTimeout(btn._t);
  btn._t = setTimeout(() => { btn.classList.remove('is-copied'); use?.setAttribute('href', '#i-copy'); }, 1500);
});

// Times: <time datetime="ISO">. Recent ones read "5 min ago", older ones a
// date; the exact local time is in the tooltip. One format everywhere.
function formatTimes(root = document) {
  const now = Date.now();
  const exact = new Intl.DateTimeFormat(undefined, {dateStyle: 'medium', timeStyle: 'short'});
  const day = new Intl.DateTimeFormat(undefined, {month: 'short', day: 'numeric'});
  const dayYear = new Intl.DateTimeFormat(undefined, {month: 'short', day: 'numeric', year: 'numeric'});
  for (const el of root.querySelectorAll('time[datetime]')) {
    const t = new Date(el.getAttribute('datetime'));
    if (isNaN(t)) continue;
    const s = Math.round((now - t) / 1000), m = Math.round(s / 60), h = Math.round(s / 3600), d = Math.floor(s / 86400);
    el.textContent = s < 45 ? 'just now' : m < 60 ? `${m} min ago` : h < 24 ? `${h}h ago` : d < 2 ? 'yesterday' : d < 7 ? `${d} days ago`
      : (t.getFullYear() === new Date().getFullYear() ? day : dayYear).format(t);
    el.title = exact.format(t);
  }
}
setInterval(formatTimes, 30000);

// Confirmation dialog (#confirm) in place of the browser's confirm(), for
// every hx-confirm. The element can add data-confirm-title,
// data-confirm-action (the button's label), data-confirm-danger, and
// data-confirm-name: a name to type before the button works.
document.addEventListener('htmx:confirm', e => {
  if (!e.detail.question) return;
  const dialog = document.getElementById('confirm');
  if (!dialog) return;
  e.preventDefault();
  const el = e.detail.elt, d = el.dataset;
  const name = d.confirmName || '';
  const ok = dialog.querySelector('[data-confirm-ok]');
  const typed = dialog.querySelector('#confirm-typed');
  const danger = el.classList.contains('btn-danger') || 'confirmDanger' in d || name !== '';
  dialog.querySelector('[data-confirm-title]').textContent = d.confirmTitle || 'Are you sure?';
  dialog.querySelector('[data-confirm-body]').textContent = e.detail.question;
  dialog.querySelector('[data-confirm-name]').textContent = name;
  dialog.querySelector('[data-confirm-typed]').hidden = !name;
  ok.textContent = d.confirmAction || el.textContent.trim() || 'Confirm';
  ok.className = 'btn ' + (danger ? 'btn-danger-solid' : 'btn-primary');
  typed.value = '';
  const sync = () => ok.disabled = name !== '' && typed.value !== name;
  typed.oninput = sync;
  sync();
  dialog.onclose = () => { if (dialog.returnValue === 'ok') e.detail.issueRequest(true); };
  dialog.returnValue = '';
  el.closest('[popover]')?.hidePopover();
  dialog.showModal();
  (name ? typed : ok).focus();
});

// Saving: a successful save reloads the page (HX-Refresh) before htmx's
// later events, so the form's id is kept when the request starts, dropped
// if it fails, and its box footer says "Saved" after the reload.
const savedKey = 'hakobu-saved';
document.addEventListener('htmx:beforeRequest', e => {
  if (!e.detail.elt.matches?.('form[id][data-saved]')) return;
  try { sessionStorage.setItem(savedKey, e.detail.elt.id); } catch (_) {}
});
document.addEventListener('htmx:afterRequest', e => {
  if (e.detail.successful) return;
  try { sessionStorage.removeItem(savedKey); } catch (_) {}
});
function showSaved() {
  let id;
  try { id = sessionStorage.getItem(savedKey); sessionStorage.removeItem(savedKey); } catch (_) { return; }
  const footer = id && document.getElementById(id)?.querySelector('.box-footer');
  if (!footer) return;
  const note = footer.querySelector('.box-footer-note');
  note?.setAttribute('hidden', '');
  footer.insertAdjacentHTML('afterbegin', '<span class="saved"><svg class="icon icon-sm"><use href="#i-check"/></svg>Saved</span>');
  setTimeout(() => { footer.querySelector('.saved')?.remove(); note?.removeAttribute('hidden'); }, 4000);
}

// Fields shown for one value of another: data-show-if="provider=s3"
// (an empty value matches an empty field).
function syncShowIf(form) {
  for (const el of form.querySelectorAll('[data-show-if]')) {
    const [name, value] = el.dataset.showIf.split('=');
    el.hidden = form.elements[name]?.value !== value;
  }
}
document.addEventListener('change', e => { if (e.target.form) syncShowIf(e.target.form); });

// New app: the name follows the repository and the subdomain follows the
// name (data-follow="name"), until either is typed in.
document.addEventListener('input', e => { e.target.dataset.edited = '1'; }, true);
document.addEventListener('change', e => {
  const form = e.target.closest('form[data-autoname]');
  if (!form) return;
  const name = form.elements.name;
  if (e.target.name === 'repo' && !name.dataset.edited) {
    name.value = e.target.value.split('/').pop().toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/^[^a-z]+/, '').slice(0, 40);
    name.dispatchEvent(new Event('follow', {bubbles: true}));
  }
});
document.addEventListener('input', followName);
document.addEventListener('follow', followName);
function followName(e) {
  const form = e.target.closest?.('form[data-autoname]');
  if (!form || e.target.name !== 'name') return;
  const sub = form.querySelector('[data-follow="name"]');
  if (sub && !sub.dataset.edited) sub.value = e.target.value;
}

// The variables editor: "KEY=value" lines as rows, or as raw .env text,
// kept in the hidden input the form sends. data-suggest offers the keys of
// the repo's .env.example.
function envEditor(root) {
  const input = root.querySelector('input[type=hidden]');
  const icon = (name, cls = '') => `<svg class="icon icon-sm ${cls}"><use href="#i-${name}"/></svg>`;
  let rows = parse(root.dataset.seed || ''), raw = false;
  root.insertAdjacentHTML('beforeend', `
    <div class="flash flash-info small mb-3" data-suggest-box hidden></div>
    <div class="space-y-2" data-rows></div>
    <textarea class="input mono" rows="8" placeholder="KEY=value" data-raw hidden></textarea>
    <div class="flex items-center justify-between mt-3">
      <button type="button" class="btn btn-sm" data-add>${icon('plus')}Add variable</button>
      <button type="button" class="btn btn-sm btn-invisible" data-mode>Edit as .env</button>
    </div>`);
  const list = root.querySelector('[data-rows]'), area = root.querySelector('[data-raw]');
  function parse(text) {
    return text.split('\n').map(l => l.trim()).filter(Boolean).map(line => {
      const i = line.indexOf('=');
      return i === -1 ? {key: line, value: ''} : {key: line.slice(0, i).trim(), value: line.slice(i + 1)};
    });
  }
  const serialize = () => rows.filter(r => r.key.trim()).map(r => r.key.trim() + '=' + r.value).join('\n');
  const sync = () => input.value = raw ? area.value : serialize();
  function draw() {
    list.replaceChildren();
    if (!rows.length) list.insertAdjacentHTML('beforeend', '<p class="field-hint">No variables yet.</p>');
    rows.forEach((r, i) => {
      const row = document.createElement('div');
      row.className = 'flex items-center gap-2';
      row.innerHTML = `<input class="input mono w-2/5" placeholder="KEY" spellcheck="false">
        <input class="input mono" type="password" placeholder="value" autocomplete="off" spellcheck="false">
        <button type="button" class="btn btn-sm btn-invisible btn-icon" title="Show">${icon('eye')}</button>
        <button type="button" class="btn btn-sm btn-invisible btn-icon btn-danger" title="Remove">${icon('trash-2')}</button>`;
      const [key, value, show, remove] = row.children;
      key.value = r.key;
      value.value = r.value;
      key.oninput = () => { r.key = key.value; sync(); };
      value.oninput = () => { r.value = value.value; sync(); };
      show.onclick = () => {
        value.type = value.type === 'password' ? 'text' : 'password';
        show.querySelector('use').setAttribute('href', value.type === 'password' ? '#i-eye' : '#i-eye-off');
      };
      remove.onclick = () => { rows.splice(i, 1); draw(); sync(); };
      list.append(row);
    });
  }
  root.querySelector('[data-add]').onclick = () => {
    rows.push({key: '', value: ''});
    draw();
    list.lastElementChild.querySelector('input').focus();
  };
  root.querySelector('[data-mode]').onclick = e => {
    if (!raw) area.value = serialize(); else { rows = parse(area.value); draw(); }
    raw = !raw;
    area.hidden = !raw;
    list.hidden = raw;
    root.querySelector('[data-add]').hidden = raw;
    e.currentTarget.textContent = raw ? 'Back to rows' : 'Edit as .env';
    sync();
  };
  area.oninput = sync;
  draw();
  if (root.dataset.suggest) {
    fetch(root.dataset.suggest).then(r => r.json()).then(data => {
      const have = new Set(rows.map(r => r.key.trim()));
      const keys = (data.keys || []).filter(k => !have.has(k));
      if (!keys.length) return;
      const box = root.querySelector('[data-suggest-box]');
      box.hidden = false;
      box.innerHTML = `<span class="muted">In <span class="mono"></span>:</span> `;
      box.querySelector('.mono').textContent = data.file || '.env.example';
      for (const k of keys) {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'btn btn-sm mono ml-1 mt-1';
        b.textContent = '+ ' + k;
        b.onclick = () => { rows.push({key: k, value: ''}); b.remove(); draw(); sync(); };
        box.append(b);
      }
    }).catch(() => {});
  }
}

// Section navigation (.subnav[data-spy]): the link of the section on screen
// is marked current.
function spy(nav) {
  const pairs = [...nav.querySelectorAll('a[href^="#"]')]
    .map(a => [a, document.getElementById(a.getAttribute('href').slice(1))]).filter(([, s]) => s);
  const mark = () => {
    let current = pairs[0];
    for (const p of pairs) if (p[1].getBoundingClientRect().top < 120) current = p;
    for (const [a] of pairs) {
      if (a === current?.[0]) a.setAttribute('aria-current', 'page');
      else a.removeAttribute('aria-current');
    }
  };
  addEventListener('scroll', mark, {passive: true});
  mark();
}

// The project canvas: each card lists the cards it uses in data-links and
// the apps it refers to in data-calls (dashed). A curve runs from its right
// edge to theirs, one port per link, or around the left side between cards
// of one column. Cards in hidden columns get no lines. Hovering a card dims
// everything it isn't linked to.
function drawLinks(canvas) {
  const svg = canvas.querySelector('.canvas-lines');
  const box = canvas.getBoundingClientRect();
  const ns = 'http://www.w3.org/2000/svg';
  svg.replaceChildren();
  const links = [];
  for (const [attr, call] of [['links', false], ['calls', true]])
    for (const from of canvas.querySelectorAll(`[data-${attr}]`))
      for (const id of from.dataset[attr].split(' ').filter(Boolean)) {
        const to = document.getElementById(id);
        if (to && canvas.contains(to) && from.offsetParent && to.offsetParent) links.push({from, to, call});
      }
  const ports = new Map();
  links.forEach((l, i) => {
    for (const el of [l.from, l.to]) {
      if (!ports.has(el)) ports.set(el, []);
      ports.get(el).push(i);
    }
  });
  // Each card's ports go in the order of the other ends, so lines don't cross needlessly.
  for (const [el, list] of ports) list.sort((a, b) => {
    const other = i => (links[i].from === el ? links[i].to : links[i].from).getBoundingClientRect().top;
    return other(a) - other(b);
  });
  const y = (el, i) => {
    const r = el.getBoundingClientRect(), list = ports.get(el), n = list.length, k = list.indexOf(i);
    const gap = Math.min(10, (r.height - 16) / Math.max(n, 1));
    return r.top - box.top + Math.min(r.height / 2, 28) + (k - (n - 1) / 2) * gap;
  };
  links.forEach(({from, to, call}, i) => {
    const a = from.getBoundingClientRect(), b = to.getBoundingClientRect();
    const g = document.createElementNS(ns, 'g');
    g.dataset.from = from.id;
    g.dataset.to = to.id;
    if (call) g.classList.add('is-call');
    let x1, x2, d;
    const y1 = y(from, i), y2 = y(to, i);
    if (Math.abs(a.left - b.left) < 32) { // one column: around its left side
      x1 = a.left - box.left;
      x2 = b.left - box.left;
      const out = Math.min(x1, x2) - 28;
      d = `M${x1},${y1} C${out},${y1} ${out},${y2} ${x2},${y2}`;
    } else {
      x1 = a.right - box.left;
      x2 = b.left - box.left;
      const dx = Math.max(40, (x2 - x1) / 2);
      d = `M${x1},${y1} C${x1 + dx},${y1} ${x2 - dx},${y2} ${x2},${y2}`;
    }
    const p = document.createElementNS(ns, 'path');
    p.setAttribute('d', d);
    g.append(p);
    for (const [cx, cy] of [[x1, y1], [x2, y2]]) {
      const c = document.createElementNS(ns, 'circle');
      c.setAttribute('cx', cx);
      c.setAttribute('cy', cy);
      c.setAttribute('r', 3);
      g.append(c);
    }
    svg.append(g);
  });
  if (canvas.dataset.focus) focusNode(canvas, document.getElementById(canvas.dataset.focus));
}

// A card or a row inside one (a domain of the tunnel, a backup) can be in
// focus; the card holding a related row counts as related too.
function focusNode(canvas, node) {
  for (const el of canvas.querySelectorAll('.is-related, .is-focus')) el.classList.remove('is-related', 'is-focus');
  if (!node) { delete canvas.dataset.focus; return; }
  canvas.dataset.focus = node.id;
  const related = el => { el.classList.add('is-related'); el.parentElement?.closest('.node')?.classList.add('is-related'); };
  node.classList.add('is-focus');
  related(node);
  // A card's own rows are its links too.
  for (const row of node.querySelectorAll('.node-item')) related(row);
  const ids = new Set([node.id, ...[...node.querySelectorAll('[id]')].map(el => el.id)]);
  for (const g of canvas.querySelectorAll('.canvas-lines g')) {
    const from = ids.has(g.dataset.from), to = ids.has(g.dataset.to);
    if (!from && !to) continue;
    g.querySelectorAll('path, circle').forEach(el => el.classList.add('is-related'));
    const other = document.getElementById(from ? g.dataset.to : g.dataset.from);
    if (other) related(other);
  }
}

function setUpCanvas(canvas) {
  new ResizeObserver(() => drawLinks(canvas)).observe(canvas);
  canvas.addEventListener('mouseover', e => {
    const node = e.target.closest('.node-item, .node');
    if (node?.id !== canvas.dataset.focus) focusNode(canvas, node);
  });
  canvas.addEventListener('mouseleave', () => focusNode(canvas, null));
}

// Set up what's in a part of the page, once per element.
function setUp(root) {
  const each = (sel, fn) => {
    const all = [...(root.matches?.(sel) ? [root] : []), ...root.querySelectorAll(sel)];
    for (const el of all) if (!el._setUp) { el._setUp = true; fn(el); }
  };
  formatTimes(root);
  markTheme(root);
  each('[data-env-editor]', envEditor);
  each('form', syncShowIf);
  each('.subnav[data-spy]', spy);
  each('.canvas', setUpCanvas);
}
htmx.onLoad(setUp);
document.addEventListener('DOMContentLoaded', showSaved);
