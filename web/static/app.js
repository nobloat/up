const $ = selector => document.querySelector(selector);

const main = () => $('main');
const searchInput = $('#q');
const onListPage = () => location.pathname === '/';

function parseHTML(html) {
  return new DOMParser().parseFromString(html, 'text/html');
}

function shortDuration(seconds) {
  if (seconds < 60) return seconds + 's';
  if (seconds < 3600) return Math.floor(seconds / 60) + 'm';
  if (seconds < 172800) return Math.floor(seconds / 3600) + 'h';
  return Math.floor(seconds / 86400) + 'd';
}

function syncOverallState() {
  const red = Number(main().dataset.red);
  const state = main().dataset.state;
  document.title = red ? `][ up (${red} failing)` : '][ up';
  $('#favicon').href = state === 'green' ? '/static/favicon.svg' : `/static/favicon-${state}.svg`;
  $('header .mark').setAttribute('class', 'mark ' + state);
}

function morph(current, next) {
  if (current.nodeName !== next.nodeName) {
    current.replaceWith(next);
    return;
  }
  if (current.nodeType !== Node.ELEMENT_NODE) {
    if (current.nodeValue !== next.nodeValue) current.nodeValue = next.nodeValue;
    return;
  }
  for (const {name} of [...current.attributes]) {
    const keep = next.hasAttribute(name) || name === 'open';
    if (!keep) current.removeAttribute(name);
  }
  for (const {name, value} of next.attributes) {
    if (current.getAttribute(name) !== value) current.setAttribute(name, value);
  }
  const currentChildren = [...current.childNodes];
  const nextChildren = [...next.childNodes];
  nextChildren.forEach((child, i) => {
    if (currentChildren[i]) morph(currentChildren[i], child);
    else current.append(child);
  });
  currentChildren.slice(nextChildren.length).forEach(child => child.remove());
}

let selected = -1;

const rows = () => [...document.querySelectorAll('tr[data-href]')];

function markSelected() {
  rows().forEach((row, i) => row.classList.toggle('sel', i === selected));
}

function select(index) {
  const all = rows();
  selected = Math.max(0, Math.min(index, all.length - 1));
  markSelected();
  all[selected]?.scrollIntoView({block: 'nearest'});
}

const disconnected = $('#conn');
let stream;
let retryDelay = 1000;

function connect() {
  stream?.close();
  const url = new URL('/events' + location.search, location.origin);
  url.searchParams.set('path', location.pathname);
  stream = new EventSource(url);

  stream.onopen = () => {
    disconnected.hidden = true;
    retryDelay = 1000;
  };
  stream.onerror = () => {
    disconnected.hidden = false;
    if (stream.readyState === EventSource.CLOSED) {
      setTimeout(connect, retryDelay);
      retryDelay = Math.min(retryDelay * 2, 10000);
    }
  };
  stream.onmessage = e => notify(JSON.parse(e.data));
  stream.addEventListener('main', e => replaceMain(e.data));
  stream.addEventListener('head', e => updateListHead(JSON.parse(e.data)));
  stream.addEventListener('row', e => updateListRow(JSON.parse(e.data)));
}

function replaceMain(html) {
  morph(main(), parseHTML(html).querySelector('main'));
  markSelected();
  syncOverallState();
}

function updateListHead({red, state, html}) {
  main().dataset.red = red;
  main().dataset.state = state;
  morph($('nav.chips'), parseHTML(html).querySelector('nav'));
  syncOverallState();
}

function updateListRow({name, html}) {
  const tbody = $('main tbody');
  const existing = [...tbody.rows].find(row => row.dataset.name === name);
  const row = html && parseHTML(`<table><tbody>${html}</tbody></table>`).querySelector('tr');
  if (!row) {
    existing?.remove();
  } else if (existing) {
    morph(existing, row);
  } else {
    const before = [...tbody.rows].find(r => Number(r.dataset.pos) > Number(row.dataset.pos));
    tbody.insertBefore(row, before || null);
  }
  markSelected();
}

addEventListener('pagehide', () => stream.close());
addEventListener('pageshow', e => {
  if (!e.persisted) return;
  disconnected.hidden = true;
  connect();
});

const bell = $('#notify');
let notificationsWanted = readSetting('notify') === 'on';

const notificationsOn = () => notificationsWanted && window.Notification && Notification.permission === 'granted';

function notify(event) {
  if (event.important && notificationsOn()) {
    new Notification('up', {body: `${event.monitor}: ${event.old} -> ${event.state}`});
  }
}

function showBell() {
  const on = notificationsOn();
  bell.classList.toggle('on', on);
  bell.setAttribute('aria-pressed', on);
  if (Notification.permission === 'denied') bell.title = 'notifications blocked in the browser settings';
  else if (on) bell.title = 'notifications on (click to mute)';
  else bell.title = 'notifications off (click to enable)';
  bell.setAttribute('aria-label', bell.title);
}

if (window.Notification) {
  bell.onclick = async () => {
    const wasOn = notificationsOn();
    if (!wasOn && Notification.permission === 'default') await Notification.requestPermission();
    notificationsWanted = !wasOn && Notification.permission === 'granted';
    writeSetting('notify', notificationsWanted ? 'on' : 'off');
    showBell();
  };
  showBell();
  bell.hidden = false;
}

const clearButton = $('#clear');
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

function showClearButton() {
  clearButton.hidden = !searchInput.value;
}

function clearSearch() {
  searchInput.value = '';
  showClearButton();
  searchInput.focus();
  if (onListPage()) search();
}

clearButton.onclick = clearSearch;

searchInput.addEventListener('input', () => {
  showClearButton();
  if (!onListPage()) return;
  clearTimeout(searchTimer);
  searchTimer = setTimeout(search, 150);
});

addEventListener('popstate', () => {
  if (!onListPage()) return;
  searchInput.value = new URL(location).searchParams.get('q') || '';
  showClearButton();
  connect();
});

document.addEventListener('click', e => {
  const filter = e.target.closest('a[href^="/?"], a[href="/"]');
  const plainClick = !e.button && !e.ctrlKey && !e.metaKey && !e.shiftKey && !e.altKey;
  if (!filter || !onListPage() || !main().contains(filter) || !plainClick) return;
  e.preventDefault();
  history.pushState(null, '', filter.href);
  selected = -1;
  connect();
});

document.addEventListener('click', e => {
  const button = e.target.closest('button.copy');
  if (!button) return;
  if (navigator.clipboard) {
    navigator.clipboard.writeText(button.dataset.copy).then(() => button.textContent = '✓');
  } else {
    getSelection().selectAllChildren(button.previousElementSibling);
  }
});

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
  endInputs.at.setCustomValidity(endChoice() === 'at' && inPast ? 'pick a time in the future' : '');

  let text = '';
  if (!endChoice()) text = 'stays paused until you resume it';
  else if (inPast) text = 'that is in the past';
  else if (end) {
    const when = end.toLocaleString([], {weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit'});
    text = `→ resumes ${when} (in ${shortDuration(seconds)})`;
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

function preparePauseSubmit() {
  const at = endInputs.at.value;
  if (endChoice() !== 'for') endInputs.for.value = '';
  dialogForm.elements.until.value = endChoice() === 'at' && at ? new Date(at).toISOString() : '';
}

function checkNow(form) {
  form.classList.add('busy');
  form.closest('tr, main')?.classList.add('checking');
  fetch(form.action, {method: 'POST'});
}

document.addEventListener('submit', e => {
  const form = e.target;
  if (form.getAttribute('role') === 'search' && onListPage()) {
    e.preventDefault();
    search();
  } else if (form.classList.contains('pause')) {
    e.preventDefault();
    openPauseDialog(form);
  } else if (form === dialogForm) {
    preparePauseSubmit();
  } else if (form.classList.contains('check')) {
    e.preventDefault();
    checkNow(form);
  }
});

function backToList() {
  if (onListPage()) return;
  const ref = document.referrer && new URL(document.referrer);
  if (ref && ref.origin === location.origin && ref.pathname === '/') history.back();
  else location.assign('/');
}

document.addEventListener('keydown', e => {
  if (dialog.open || e.ctrlKey || e.metaKey || e.altKey) return;

  if (e.target.matches('input, select, textarea')) {
    if (e.key !== 'Escape') return;
    if (e.target === searchInput && searchInput.value) clearSearch();
    else e.target.blur();
    return;
  }

  const all = rows();
  const scope = all.length ? all[selected] : main();
  const shortcuts = {
    '/': () => searchInput.focus(),
    j: () => select(selected + 1),
    k: () => select(selected - 1),
    Enter: () => { if (all[selected]) location.href = all[selected].dataset.href; },
    c: () => scope?.querySelector('form.check')?.requestSubmit(),
    p: () => scope?.querySelector('form.pause, form.resume')?.requestSubmit(),
    Escape: backToList,
  };
  const action = shortcuts[e.key];
  if (!action) return;
  e.preventDefault();
  action();
});

const themeButton = $('#theme');
const root = document.documentElement;
const themes = {
  '': {icon: '◐', name: 'auto', next: 'light'},
  light: {icon: '☀︎', name: 'light', next: 'dark'},
  dark: {icon: '☾', name: 'dark', next: ''},
};
const currentTheme = () => themes[root.dataset.theme || ''];

function showTheme() {
  const theme = currentTheme();
  themeButton.textContent = theme.icon;
  themeButton.title = `theme: ${theme.name} (click: ${themes[theme.next].name})`;
  themeButton.setAttribute('aria-label', `color theme: ${theme.name}`);
}

themeButton.onclick = () => {
  const next = currentTheme().next;
  if (next) root.dataset.theme = next;
  else delete root.dataset.theme;
  writeSetting('theme', next);
  showTheme();
};
showTheme();
themeButton.hidden = false;

function readSetting(key) {
  try { return localStorage.getItem(key); } catch { return null; }
}

function writeSetting(key, value) {
  try {
    if (value) localStorage.setItem(key, value);
    else localStorage.removeItem(key);
  } catch {}
}

function updateRelativeTimes() {
  for (const el of document.querySelectorAll('time[data-rel]')) {
    const seconds = Math.round((new Date(el.dateTime) - Date.now()) / 1000);
    const due = seconds === 0 || (seconds < 0 && el.dataset.rel === 'next');
    el.textContent = due ? 'now' : (seconds < 0 ? '−' : '+') + shortDuration(Math.abs(seconds));
  }
  if (dialog.open) describePauseEnd();
}

setInterval(updateRelativeTimes, 1000);
showClearButton();
syncOverallState();
connect();
