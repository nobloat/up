// up's own behavior; nobloat/ui's ui.js (loaded before) does theme, keys, palette, dialogs, autosave, copy and morph.
// Live updates: /events streams the page's #live block, list rows and the chips as the monitors change.
const $ = selector => document.querySelector(selector);
const T = window.T;
const live = () => $('#live');
const searchInput = $('#q');
const onListPage = () => location.pathname === '/';

const parseHTML = html => new DOMParser().parseFromString(html, 'text/html');

function shortDuration(seconds) {
  if (seconds < 60) return seconds + 's';
  if (seconds < 3600) return Math.floor(seconds / 60) + 'm';
  if (seconds < 172800) return Math.floor(seconds / 3600) + 'h';
  return Math.floor(seconds / 86400) + 'd';
}

// title and favicon follow the worst state; the logo's dot follows it in app.css
function syncOverallState() {
  const el = live();
  if (!el) return;
  const red = Number(el.dataset.red), state = el.dataset.state;
  const base = `${document.title.split(' · ')[0]} · ${el.dataset.title}`;
  document.title = red ? `${base} (${T['js.up.failing'].replace('{n}', red)})` : base;
  $('link[rel=icon]').href = state === 'green' ? '/static/favicon.svg' : `/static/favicon-${state}.svg`;
}

// after a re-render: phones show cells as "label … value" (ui.js labels them once at load), and the row selected with j/k stays selected
function relabel(selected) {
  if (selected) document.getElementById(selected)?.classList.add('sel');
  document.querySelectorAll('#live table:not(.keep)').forEach(t => {
    const heads = [...t.querySelectorAll('thead th')].map(th => th.textContent.trim());
    t.querySelectorAll('tbody tr').forEach(tr => [...tr.cells].forEach((td, i) => td.dataset.label = heads[i] ?? ''));
  });
}

// morph keeps focus; open <details> stay open across a re-render
function patch(current, next) {
  current.querySelectorAll('details[open][id]').forEach(d => next.querySelector('#' + CSS.escape(d.id))?.setAttribute('open', ''));
  const selected = $('#live tr.sel')?.id;
  morph(current, next);
  relabel(selected);
}

// ---- live updates ------------------------------------------------------------------------------
const conn = document.createElement('div');
conn.className = 'conn';
conn.setAttribute('role', 'alert');
conn.textContent = T['js.up.conn'];
conn.hidden = true;
document.body.prepend(conn);

let stream;
let retryDelay = 1000;

function connect() {
  if (!live()) return; // pages without live content (edit, settings) need no stream
  stream?.close();
  const url = new URL('/events' + location.search, location.origin);
  url.searchParams.set('path', location.pathname);
  stream = new EventSource(url);
  stream.onopen = () => {
    conn.hidden = true;
    retryDelay = 1000;
  };
  stream.onerror = () => {
    conn.hidden = false;
    if (stream.readyState === EventSource.CLOSED) {
      setTimeout(connect, retryDelay);
      retryDelay = Math.min(retryDelay * 2, 10000);
    }
  };
  stream.onmessage = e => notify(JSON.parse(e.data));
  stream.addEventListener('main', e => {
    patch(live(), parseHTML(e.data).querySelector('#live'));
    syncOverallState();
  });
  stream.addEventListener('head', e => {
    const {red, state, html} = JSON.parse(e.data);
    live().dataset.red = red;
    live().dataset.state = state;
    morph($('nav.chips'), parseHTML(html).querySelector('nav'));
    syncOverallState();
  });
  stream.addEventListener('row', e => {
    const {name, html} = JSON.parse(e.data);
    const tbody = $('#live tbody');
    const existing = [...tbody.rows].find(row => row.dataset.name === name);
    const selected = $('#live tr.sel')?.id;
    const row = html && parseHTML(`<table><tbody>${html}</tbody></table>`).querySelector('tr');
    if (!row) existing?.remove();
    else if (existing) morph(existing, row);
    else tbody.insertBefore(row, [...tbody.rows].find(r => Number(r.dataset.pos) > Number(row.dataset.pos)) || null);
    relabel(selected);
  });
}

addEventListener('pagehide', () => stream?.close());

// ---- browser notifications: the toggle lives on the settings page and in the palette -------------
const readSetting = key => { try { return localStorage.getItem(key); } catch { return null; } };
const writeSetting = (key, value) => { try { value ? localStorage.setItem(key, value) : localStorage.removeItem(key); } catch {} };
let notificationsWanted = readSetting('notify') === 'on';
const notificationsOn = () => notificationsWanted && window.Notification && Notification.permission === 'granted';

function notify(event) {
  if (event.important && notificationsOn()) new Notification('up', {body: `${event.monitor}: ${event.old} → ${event.state}`});
}

const bell = $('#notify');
function showBell() {
  if (!bell) return;
  const on = notificationsOn();
  bell.setAttribute('aria-pressed', on);
  bell.disabled = !window.Notification || Notification.permission === 'denied';
  bell.querySelector('span').textContent = !window.Notification ? T['js.up.notify.unsupported']
    : Notification.permission === 'denied' ? T['js.up.notify.blocked'] : T[on ? 'js.up.notify.on' : 'js.up.notify.off'];
}

async function toggleNotifications() {
  if (!window.Notification) return;
  const wasOn = notificationsOn();
  if (!wasOn && Notification.permission === 'default') await Notification.requestPermission();
  notificationsWanted = !wasOn && Notification.permission === 'granted';
  writeSetting('notify', notificationsWanted ? 'on' : 'off');
  showBell();
}

if (bell) bell.onclick = toggleNotifications;
showBell();
if (window.Notification) window.commands = [...(window.commands || []), {label: T['js.up.notify.cmd'], run: toggleNotifications}];

// ---- search: on the list, typing filters live (the URL keeps q, state and tags) -----------------------
let searchTimer;
function search() {
  clearTimeout(searchTimer);
  const url = new URL(location);
  const q = searchInput.value.trim();
  if (q) url.searchParams.set('q', q);
  else url.searchParams.delete('q');
  history.replaceState(null, '', url);
  connect();
}

if (searchInput) {
  searchInput.addEventListener('input', () => {
    if (!onListPage()) return;
    clearTimeout(searchTimer);
    searchTimer = setTimeout(search, 150);
  });
  searchInput.addEventListener('keydown', e => { // Esc empties the search first, then leaves the field
    if (e.key !== 'Escape' || !searchInput.value) return;
    e.stopPropagation();
    searchInput.value = '';
    if (onListPage()) search();
  });
  searchInput.form.addEventListener('submit', e => {
    if (!onListPage()) return;
    e.preventDefault();
    search();
  });
}

addEventListener('popstate', () => {
  if (!onListPage()) return;
  if (searchInput) searchInput.value = new URL(location).searchParams.get('q') || '';
  connect();
});

// filter chips and tags on the list swap the list in place
document.addEventListener('click', e => {
  const filter = e.target.closest('a[href^="/?"], a[href="/"]');
  const plainClick = !e.button && !e.ctrlKey && !e.metaKey && !e.shiftKey && !e.altKey;
  if (!filter || !onListPage() || !live()?.contains(filter) || !plainClick) return;
  e.preventDefault();
  history.pushState(null, '', filter.href);
  connect();
});

// ---- pause dialog: reason and an optional end ------------------------------------------------------
const dialog = $('#pause-dialog');
const dialogForm = dialog.querySelector('form');
const endChoice = () => dialogForm.elements.end.value;
const endInputs = {for: dialogForm.elements.for, at: $('#pause-at')};
const durationUnits = {d: 864e5, h: 36e5, m: 6e4, s: 1e3};

function parseDurationMs(text) {
  let ms = 0;
  for (const [, n, unit] of text.matchAll(/([\d.]+)([dhms])/g)) ms += n * durationUnits[unit];
  return ms;
}

function pauseEnd() {
  const value = endInputs[endChoice()]?.value;
  if (!value) return null;
  if (endChoice() === 'at') return new Date(value);
  if (!endInputs.for.checkValidity()) return null;
  return new Date(Date.now() + parseDurationMs(value));
}

function describePauseEnd() {
  const end = pauseEnd();
  const seconds = end && Math.round((end - Date.now()) / 1000);
  const inPast = end && seconds <= 0;
  endInputs.at.setCustomValidity(endChoice() === 'at' && inPast ? T['js.up.pause.future'] : '');
  let text = '';
  if (!endChoice()) text = T['js.up.pause.manual'];
  else if (inPast) text = T['js.up.pause.past'];
  else if (end) {
    const when = end.toLocaleString([], {weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit'});
    text = '→ ' + T['js.up.pause.resumes'].replace('{when}', when).replace('{in}', shortDuration(seconds));
  }
  $('#pause-end').textContent = text;
}

function pickEnd() {
  for (const [choice, input] of Object.entries(endInputs)) input.required = endChoice() === choice;
  describePauseEnd();
}

function openPauseDialog(pauseForm) {
  dialogForm.action = pauseForm.action;
  $('#pause-name').textContent = pauseForm.dataset.name;
  dialogForm.reset();
  pickEnd();
  dialog.showModal();
}

dialog.querySelector('.ends').onchange = pickEnd;
dialog.querySelector('.ends').oninput = describePauseEnd;
for (const [choice, input] of Object.entries(endInputs)) {
  input.onfocus = () => {
    dialogForm.elements.end.value = choice;
    pickEnd();
  };
}

function checkNow(form) {
  form.querySelector('button').setAttribute('aria-busy', 'true');
  fetch(form.action, {method: 'POST'});
}

// capture: runs before ui.js's spinner and confirmation
document.addEventListener('submit', e => {
  const form = e.target;
  if (form.classList.contains('pause')) {
    e.preventDefault();
    e.stopImmediatePropagation();
    openPauseDialog(form);
  } else if (form === dialogForm) {
    const at = endInputs.at.value;
    if (endChoice() !== 'for') endInputs.for.value = '';
    dialogForm.elements.until.value = endChoice() === 'at' && at ? new Date(at).toISOString() : '';
  } else if (form.classList.contains('check')) {
    e.preventDefault();
    e.stopImmediatePropagation();
    checkNow(form);
  }
}, true);

// ---- new: the type picks a template ----------------------------------------------------------------
document.addEventListener('change', e => {
  const select = e.target;
  if (select.id !== 'conf-type') return;
  const conf = select.form.conf;
  const untouched = [...select.options].some(o => o.dataset.conf === conf.value);
  if (untouched || confirm(T['js.up.replace'].replace('{type}', select.value))) conf.value = select.selectedOptions[0].dataset.conf;
  else select.value = conf.dataset.type ?? select.options[0].value;
  conf.dataset.type = select.value;
  select.form.kind.value = select.selectedOptions[0].dataset.kind;
});

// ---- keys: r check now, p pause or resume (the selected row, else the page); Ctrl+Enter saves -----------
const scope = () => $('#live tr.sel') || live();
window.pageKeys = {
  r: () => scope()?.querySelector('form.check')?.requestSubmit(),
  p: () => { const f = scope()?.querySelector('form.pause, form.resume'); if (!f) return false; f.requestSubmit(); },
  'C-Enter': () => {
    const f = $('form.edit');
    if (!f) return false;
    if (f.matches('[data-autosave]')) f.conf.dispatchEvent(new Event('change', {bubbles: true}));
    else f.requestSubmit();
  },
};

function updateRelativeTimes() {
  for (const el of document.querySelectorAll('time[data-rel]')) {
    const seconds = Math.round((new Date(el.dateTime) - Date.now()) / 1000);
    const due = seconds === 0 || (seconds < 0 && el.dataset.rel === 'next');
    el.textContent = due ? 'now' : (seconds < 0 ? '−' : '+') + shortDuration(Math.abs(seconds));
  }
  if (dialog.open) describePauseEnd();
}

setInterval(updateRelativeTimes, 1000);
syncOverallState();
connect();
