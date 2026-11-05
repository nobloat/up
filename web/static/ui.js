// nobloat/ui v1: copy unchanged to web/static/ui.js; app-specific code goes in app.js (loaded after).
// Progressive enhancement only: every page works without it. Texts come from <script id="texts" type="application/json">.
const $ = (s, el = document) => el.querySelector(s);
window.T = JSON.parse(document.getElementById('texts')?.textContent || '{}');
const esc = s => String(s).replace(/[&<>"]/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;'})[c]);

// confirmation for destructive or public actions: <form data-confirm="text" data-confirm-title="Delete X?">
// (or on the submit button); the title is optional; the submit button shows a spinner
window.confirmDlg = (text, title) => new Promise(res => {
  const d = $('#confirm');
  if (!d) return res(confirm((title ? title + ': ' : '') + text));
  $('#confirm-text').textContent = text;
  const h = $('#confirm-title'); h.textContent = title || ''; h.hidden = !title;
  d.returnValue = '';
  d.onclose = () => res(d.returnValue === 'yes');
  d.showModal();
});
document.addEventListener('submit', async e => {
  const f = e.target;
  if (f.method === 'dialog') return;
  const ask = e.submitter?.dataset.confirm || f.dataset.confirm; // a button may carry its own question (bulk bar)
  if (ask && !f.dataset.confirmed) {
    e.preventDefault();
    if (await confirmDlg(ask, e.submitter?.dataset.confirmTitle || f.dataset.confirmTitle)) { f.dataset.confirmed = 1; f.requestSubmit(e.submitter); }
    return;
  }
  delete f.dataset.confirmed;
  if (f.target !== '_blank') e.submitter?.setAttribute('aria-busy', 'true');
});

// theme: auto (follows the OS), light or dark; the header toggle cycles, the settings page has radios (#theme-seg);
// stored in localStorage, the <head> script applies it before first paint
{
  const b = $('#theme'), root = document.documentElement, radios = document.querySelectorAll('input[name=nb-theme]');
  const names = {'': ['contrast', T['js.theme.auto'] || 'auto'], light: ['sun', T['js.theme.light'] || 'light'], dark: ['moon', T['js.theme.dark'] || 'dark']};
  const show = () => {
    const t = root.dataset.theme || '';
    if (b) { b.innerHTML = `<i class="icon-${names[t][0]}" aria-hidden="true"></i>`; b.title = (T['js.theme'] || 'theme') + ': ' + names[t][1]; b.setAttribute('aria-label', b.title); b.hidden = false; }
    radios.forEach(r => r.checked = r.value === t);
  };
  const set = t => {
    t ? root.dataset.theme = t : delete root.dataset.theme;
    try { t ? localStorage.setItem('theme', t) : localStorage.removeItem('theme'); } catch {}
    show();
  };
  if (b) b.onclick = () => set(({'': 'light', light: 'dark', dark: ''})[root.dataset.theme || '']);
  radios.forEach(r => r.onchange = () => set(r.value));
  show();
  const m = $('#menu'), nav = $('#nav');
  if (m && nav) m.onclick = () => { const open = nav.classList.toggle('open'); m.setAttribute('aria-expanded', open); m.innerHTML = `<i class="icon-${open ? 'x' : 'menu'}" aria-hidden="true"></i>`; };
  // nav shortcut letters: underline the first matching letter of each label
  document.querySelectorAll('nav.app a[data-g]').forEach(a => {
    const t = a.textContent, i = t.toLowerCase().indexOf(a.dataset.g);
    if (i >= 0) a.innerHTML = esc(t.slice(0, i)) + '<u>' + esc(t[i]) + '</u>' + esc(t.slice(i + 1));
  });
}

// live search: <form class="gsearch" data-api="/api/search"> gets {"group": [{url, title, sub}]}; without data-api it is a plain GET form
{
  const q = $('#q'), box = $('#results'), api = q?.form?.dataset.api;
  if (q && box && api) {
    let t, sel = -1;
    const items = () => [...box.querySelectorAll('a')];
    const mark = () => items().forEach((a, i) => a.classList.toggle('sel', i === sel));
    const run = async () => {
      const v = q.value.trim();
      if (!v) { box.hidden = true; return; }
      const r = await fetch(api + (api.includes('?') ? '&' : '?') + 'limit=8&q=' + encodeURIComponent(v));
      if (!r.ok || q.value.trim() !== v) return;
      let html = '';
      for (const [grp, hits] of Object.entries(await r.json())) {
        if (!hits.length) continue;
        html += `<div class="grp">${esc(T['search.' + grp] || grp)}</div>`;
        for (const h of hits) html += `<a href="${esc(h.url)}"><span>${esc(h.title || h.id)}</span><span class="sub">${esc(h.sub || '')}</span></a>`;
      }
      box.innerHTML = html || `<div class="none">${esc(T['search.none'] || '—')}</div>`;
      box.hidden = false;
      sel = items().length ? 0 : -1;
      mark();
    };
    q.addEventListener('input', () => { clearTimeout(t); t = setTimeout(run, 120); });
    q.addEventListener('keydown', e => {
      const it = items();
      if (e.key === 'ArrowDown' || (e.ctrlKey && e.key === 'j')) { e.preventDefault(); sel = Math.min(sel + 1, it.length - 1); mark(); }
      else if (e.key === 'ArrowUp' || (e.ctrlKey && e.key === 'k')) { e.preventDefault(); sel = Math.max(sel - 1, 0); mark(); }
      else if (e.key === 'Enter' && it[sel] && !box.hidden) { e.preventDefault(); location = it[sel].href; }
      else if (e.key === 'Escape') { box.hidden = true; q.value ? q.value = '' : q.blur(); e.stopPropagation(); }
    });
    q.addEventListener('blur', () => setTimeout(() => box.hidden = true, 150));
  }
}

// multi-value autocomplete (tags): <input data-pick="tags"> + <datalist id="tags"><option value="#name" label="Label">.
// Space-separated tokens; name and label match anywhere, prefix matches first, used tokens are left out;
// ↑/↓ choose, Tab/Enter pick, Esc closes. Single values need no JS: <input list="tags"> is native.
{
  let box;
  const close = () => { box?.remove(); box = null; };
  const open = el => {
    close();
    const m = el.value.slice(0, el.selectionStart).match(/(^|\s)(\S*)$/), q = m[2].toLowerCase();
    const used = new Set(el.value.toLowerCase().split(/\s+/));
    const hits = [...document.querySelectorAll(`#${CSS.escape(el.dataset.pick)} option`)]
      .map(o => ({v: o.value, label: o.label || o.value, at: Math.min(...[o.value, o.label].map(s => s.toLowerCase().indexOf(q.replace(/^#/, ''))).filter(i => i >= 0))}))
      .filter(t => t.at < Infinity && (!used.has(t.v.toLowerCase()) || t.v.toLowerCase() === q))
      .sort((a, b) => (a.at > 1) - (b.at > 1) || a.v.localeCompare(b.v)).slice(0, 8);
    if (!hits.length) return;
    box = document.createElement('div');
    box.className = 'results pick';
    box.innerHTML = hits.map(t => `<a href="#" data-v="${esc(t.v)}"><span>${esc(t.label)}</span><span class="sub">${t.label === t.v ? '' : esc(t.v)}</span></a>`).join('');
    const r = el.getBoundingClientRect();
    box.style.cssText = `left:${r.left + scrollX}px;top:${r.bottom + scrollY + 2}px`;
    if (q) box.firstChild.classList.add('sel'); // Enter picks only while typing a token
    box.el = el;
    document.body.append(box);
    const start = el.selectionStart - m[2].length;
    const pick = v => {
      const rest = el.value.slice(el.selectionStart).replace(/^\S*\s*/, '');
      el.value = el.value.slice(0, start) + v + ' ' + rest;
      el.setSelectionRange(start + v.length + 1, start + v.length + 1);
      el.dispatchEvent(new Event('input', {bubbles: true}));
    };
    box.onmousedown = e => { e.preventDefault(); const a = e.target.closest('a'); if (a) pick(a.dataset.v); };
  };
  const ours = el => el.matches?.('input[data-pick]:not([readonly])');
  document.addEventListener('input', e => { if (ours(e.target)) open(e.target); });
  document.addEventListener('focusin', e => { if (ours(e.target)) open(e.target); });
  document.addEventListener('focusout', e => { if (ours(e.target)) setTimeout(() => { if (box?.el !== document.activeElement) close(); }, 150); });
  document.addEventListener('keydown', e => {
    if (!box || e.target !== box.el) return;
    const as = [...box.querySelectorAll('a')], i = as.findIndex(a => a.classList.contains('sel'));
    if ((e.key === 'Tab' || e.key === 'Enter') && i >= 0) { e.preventDefault(); e.stopImmediatePropagation(); box.onmousedown({preventDefault() {}, target: as[i]}); }
    else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      const down = e.key === 'ArrowDown';
      as[i]?.classList.remove('sel');
      as[i < 0 ? (down ? 0 : as.length - 1) : (i + (down ? 1 : as.length - 1)) % as.length].classList.add('sel');
    }
    else if (e.key === 'Escape') { e.stopImmediatePropagation(); close(); }
  }, true);
}

// money: <input data-money="1.234,56"> takes both conventions (1.234,56 · 1,234.56 · 1234.5 · -12) and rewrites the value
// in the format of the example before it is sent; nothing else is accepted. money.parse(s) → cents or NaN, money.format(cents, example).
window.money = {
  parse: s => {
    s = String(s).trim().replace(/[\s'€$£]|EUR|CHF|USD/g, '').replace('−', '-');
    const neg = s.startsWith('-'); s = s.replace(/^[-+]/, '');
    const m = s.match(/^(\d{1,3}(?:([.,])\d{3})*|\d+)(?:([.,])(\d{1,2}))?$/);
    if (!m || (m[2] && m[3] === m[2])) return NaN;
    const cents = +m[1].replace(/[.,]/g, '') * 100 + +(m[4] || '0').padEnd(2, '0');
    return neg ? -cents : cents;
  },
  format: (cents, ex = '1.234,56', signed = false) => { // signed: +1.240,00 / −1.240,00 (with .money.pos / .neg)
    const group = ex.match(/^1(\D?)234/)[1], dec = ex.match(/4(\D)56$/)[1];
    const a = Math.abs(cents), int = String(Math.floor(a / 100)).replace(/\B(?=(\d{3})+(?!\d))/g, group);
    return (cents < 0 ? '−' : signed && cents > 0 ? '+' : '') + int + dec + String(a % 100).padStart(2, '0');
  },
};
document.addEventListener('change', e => { // capture: runs before autosave sends the form
  const el = e.target;
  if (!el.matches?.('input[data-money]') || !el.value.trim()) return;
  const c = money.parse(el.value), ex = el.dataset.money || '1.234,56';
  if (Number.isNaN(c)) { el.setCustomValidity(T['js.invalid'] || 'not valid'); fieldError(el, (T['js.invalid'] || 'not valid') + ': ' + el.value, (T['js.money.hint'] || 'an amount like') + ' ' + ex); e.stopImmediatePropagation(); return; }
  el.setCustomValidity('');
  el.value = money.format(c, ex);
}, true);

// field errors: the field gets aria-invalid and, right under it, <span class="err">what is wrong<span class="valid">what is valid</span></span>.
// The server renders the same markup without JS. The hint comes from the answer, data-hint or the field's title.
window.fieldError = (el, msg, hint) => {
  const id = (el.id || el.name) + '-err';
  let e = document.getElementById(id);
  if (!msg) { e?.remove(); el.removeAttribute('aria-invalid'); el.setAttribute('aria-describedby', (el.getAttribute('aria-describedby') || '').replace(id, '').trim()); return; }
  if (!e) { e = document.createElement('span'); e.className = 'err'; e.id = id; e.setAttribute('role', 'alert'); (el.closest('.form > label:has(input)') || el.closest('.range, .seg') || el).after(e); }
  e.innerHTML = esc(msg.replace(/[.\s]+$/, '')) + ((hint = hint || el.dataset.hint || el.title) ? `<span class="valid">${esc(hint)}</span>` : ''); // one line: what is wrong: what is valid
  el.setAttribute('aria-invalid', 'true');
  if (!(el.getAttribute('aria-describedby') || '').includes(id)) el.setAttribute('aria-describedby', ((el.getAttribute('aria-describedby') || '') + ' ' + id).trim());
};
// the browser's own checks (required, pattern, type, min/max) show the same way instead of a bubble
// short words instead of the browser's long sentences when the field says what is valid (title, data-hint)
document.addEventListener('invalid', e => { const el = e.target; e.preventDefault(); fieldError(el, el.validity.valueMissing ? (T['js.required'] || 'required') : (el.title || el.dataset.hint) ? (T['js.invalid'] || 'not valid') : el.validationMessage); }, true);
document.addEventListener('input', e => { if (e.target.getAttribute?.('aria-invalid') && e.target.checkValidity()) fieldError(e.target); });

// autosave: <form data-autosave> posts the form on every change (Accept: application/json). The answer is {rev?}
// or 4xx {error, field?, hint?}. Saved: the field's edge flashes once and .saved says "✓ saved 14:02" (aria-live), no toast.
// Invalid fields are not sent. The submit button (.nojs) stays the no-JS path. "saved" fires for pages that re-render.
document.addEventListener('change', async e => {
  const el = e.target, f = el.form;
  if (!f?.matches('[data-autosave]')) return;
  if (!el.checkValidity()) return; // the invalid event above already shows why
  const s = $('.saved', f) || $('.saved');
  if (s) s.textContent = T['js.saving'] || 'saving…';
  const r = await fetch(f.action, {method: 'POST', body: new URLSearchParams(new FormData(f)), headers: {Accept: 'application/json'}}).catch(() => null);
  const j = await r?.json().catch(() => ({})) || {};
  if (r?.ok) {
    if (f.elements.rev && j.rev) f.elements.rev.value = j.rev;
    fieldError(el);
    el.classList.add('was-saved'); setTimeout(() => el.classList.remove('was-saved'), 1200);
    if (s) { s.textContent = '✓ ' + (T['js.saved'] || 'saved') + ' ' + new Date().toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'}); s.classList.remove('err'); }
    f.dispatchEvent(new CustomEvent('saved', {detail: j}));
  } else if (r?.status === 401) { // signed out meanwhile: keep the input, say so once
    document.body.dataset.sessionEnd = new Date(0).toISOString();
    window.sessionCheck();
    if (s) s.textContent = '';
  } else {
    const msg = T['err.' + j.error] || j.error || T['js.error'] || 'error';
    fieldError(f.elements[j.field] || el, msg, j.hint);
    if (s) { s.textContent = ''; }
  }
});

// keyboard, vim style. Page handlers first (window.pageKeys = {key: fn}, return false to fall through), then:
// g+letter nav · / search · ? help · Ctrl+K or : palette · j/k rows · Enter open · h/l tabs · p/n pages · 1-9 chips · Esc up · [data-key] click
{
  let g = false, sel = -1;
  const rows = () => [...document.querySelectorAll('main tr[data-href]')];
  // the selected row is part of the URL (#CUS-0042, the row's id), so reload, Back and a shared link show the same selection
  const mark = () => { rows().forEach((r, i) => r.classList.toggle('sel', i === sel)); const id = rows()[sel]?.id; if (id) history.replaceState(history.state, '', '#' + id); };
  { const i = location.hash.length > 1 ? rows().findIndex(r => r.id === decodeURIComponent(location.hash.slice(1))) : -1; if (i >= 0) { sel = i; rows()[i].classList.add('sel'); rows()[i].scrollIntoView({block: 'center'}); } }
  const visible = s => [...document.querySelectorAll(s)].filter(el => el.offsetParent !== null);
  const go = el => { if (el) location = el.href; };
  const keyName = e => (e.ctrlKey || e.metaKey ? 'C-' : '') + (e.altKey ? 'A-' : '') + (e.altKey ? e.code.replace(/^Key/, '').toLowerCase() : e.key);
  document.addEventListener('keydown', async e => {
    if (document.querySelector('dialog[open]')) return;
    const k = keyName(e);
    if (e.target.matches('input, select, textarea, [contenteditable]')) {
      if (k === 'Escape') { e.target.blur(); e.preventDefault(); }
      else if (window.pageKeys?.[k] && /^[AC]-/.test(k)) { e.preventDefault(); window.pageKeys[k](e); }
      return;
    }
    if (g) { g = false; const a = $(`nav.app a[data-g="${CSS.escape(e.key)}"]`); if (a) { e.preventDefault(); go(a); } return; }
    if (window.pageKeys?.[k]) { e.preventDefault(); if (await window.pageKeys[k](e) !== false) return; }
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    const r = rows(), tabs = visible('.tabs a'), cur = tabs.findIndex(a => a.classList.contains('on'));
    const step = d => { sel = Math.max(0, Math.min(sel + d, r.length - 1)); mark(); r[sel]?.scrollIntoView({block: 'nearest'}); };
    const keys = {
      g: () => g = true,
      '/': () => $('#q')?.focus(),
      '?': () => $('#help')?.showModal(),
      j: () => step(1), k: () => step(-1),
      Enter: () => { if (r[sel]) location = r[sel].dataset.href; },
      h: () => go(tabs[cur - 1]), l: () => go(tabs[cur + 1]),
      p: () => go($('.pager a[rel=prev]:not(.off)')), n: () => go($('.pager a[rel=next]:not(.off)')), // letters: same key on every layout (no AltGr on de/ISO)
      Escape: () => { const on = [...document.querySelectorAll('td.pick input:checked')]; if (on.length) { on.forEach(b => b.checked = false); on[0].dispatchEvent(new Event('change', {bubbles: true})); } else go([...document.querySelectorAll('a.up')].pop()); }, // clear the selection, else one breadcrumb up
    };
    const chip = /^[1-9]$/.test(e.key) && visible('main .chips a.chip')[+e.key - 1];
    const el = visible(`[data-key="${CSS.escape(e.key)}"]`)[0];
    if (el) { e.preventDefault(); el.matches('input, select, textarea') ? el.focus() : el.click(); }
    else if (chip) { e.preventDefault(); go(chip); }
    else if (keys[e.key]) { e.preventDefault(); keys[e.key](); }
  });
  document.addEventListener('click', e => {
    const tr = e.target.closest('tr[data-href]');
    if (tr && !e.target.closest('a, button, input, select, label')) location = tr.dataset.href;
  });
}

// uploads: <label class="drop"> around an <input type="file" accept=… data-max=bytes>. While files are dragged over the window
// every zone shows .ready; over a zone .over ("release to add n files"). Dropped or chosen files are checked against accept
// and data-max (refused ones get a field error), listed with their size, and the input fires change (autosave, forms).
{
  const zones = () => [...document.querySelectorAll('label.drop')];
  const size = b => b < 1024 ? b + ' B' : b < 1048576 ? Math.round(b / 1024) + ' KB' : (b / 1048576).toFixed(1) + ' MB';
  const fits = (f, accept) => !accept || accept.split(',').map(a => a.trim().toLowerCase()).some(a =>
    a.startsWith('.') ? f.name.toLowerCase().endsWith(a) : a.endsWith('/*') ? f.type.startsWith(a.slice(0, -1)) : f.type === a);
  const take = (zone, files) => {
    const inp = $('input[type=file]', zone), max = +inp.dataset.max || Infinity, dt = new DataTransfer(), bad = [];
    for (const f of files) (fits(f, inp.accept) && f.size <= max ? dt.items.add(f) : bad.push(f.name + (f.size > max ? ' (' + size(f.size) + ')' : '')));
    if (!inp.multiple) while (dt.items.length > 1) dt.items.remove(1);
    inp.files = dt.files;
    let list = $('.files', zone);
    if (!list) { list = document.createElement('ul'); list.className = 'files'; zone.append(list); }
    list.innerHTML = [...dt.files].map(f => `<li><i class="icon-file" aria-hidden="true"></i>${esc(f.name)} <span class="muted">${size(f.size)}</span></li>`).join('');
    fieldError(inp, bad.length ? (T['js.drop.refused'] || 'not supported') + ': ' + bad.join(', ') : '', bad.length ? $('small', zone)?.textContent : '');
    inp.dispatchEvent(new Event('change', {bubbles: true}));
  };
  let depth = 0;
  const has = e => [...(e.dataTransfer?.types || [])].includes('Files');
  addEventListener('dragenter', e => { if (has(e) && depth++ === 0) zones().forEach(z => z.classList.add('ready')); });
  addEventListener('dragleave', e => { if (has(e) && --depth === 0) zones().forEach(z => z.classList.remove('ready', 'over')); });
  addEventListener('dragover', e => {
    if (!has(e)) return;
    e.preventDefault(); // a drop outside a zone must not open the file
    const z = e.target.closest?.('label.drop');
    zones().forEach(x => x.classList.toggle('over', x === z));
    if (z) { e.dataTransfer.dropEffect = 'copy'; const n = e.dataTransfer.items.length; $('.hint-drop', z) ?? z.append(Object.assign(document.createElement('b'), {className: 'hint-drop'})); $('.hint-drop', z).textContent = (T['js.drop.release'] || 'release to add {n} files').replace('{n}', n); }
  });
  addEventListener('drop', e => {
    if (!has(e)) return;
    e.preventDefault();
    depth = 0;
    const z = e.target.closest?.('label.drop');
    zones().forEach(x => { x.classList.remove('ready', 'over'); $('.hint-drop', x)?.remove(); });
    if (z) take(z, e.dataTransfer.files);
  });
  document.addEventListener('change', e => { const z = e.target.closest?.('label.drop'); if (z && e.isTrusted) take(z, e.target.files); });
}

// overflow menus close on a click anywhere else and on Esc (a <details> stays open by itself)
document.addEventListener('click', e => document.querySelectorAll('details.more[open]').forEach(d => { if (!d.contains(e.target)) d.open = false; }));
document.addEventListener('keydown', e => { if (e.key !== 'Escape') return; const d = document.querySelector('details.more[open]'); if (d) { d.open = false; d.querySelector('summary').focus(); e.stopImmediatePropagation(); } }, true);

// overflow menus open towards the side with room
document.addEventListener('toggle', e => {
  const d = e.target, m = d.matches?.('details.more') && d.open && $('.menu', d);
  if (m) { m.classList.remove('left'); if (m.getBoundingClientRect().left < 0) m.classList.add('left'); }
}, true);

// a tag shortened with … shows its full name on hover
document.querySelectorAll('.tag').forEach(t => { if (!t.title && t.scrollWidth > t.clientWidth) t.title = t.textContent.trim(); });

// tables: each cell gets its column header as data-label, which phones show as "label … value" lines
document.querySelectorAll('main table:not(.keep)').forEach(t => {
  const heads = [...t.querySelectorAll('thead th')].map(th => th.textContent.trim());
  t.querySelectorAll('tbody tr, tfoot tr').forEach(tr => [...tr.cells].forEach((td, i) => td.dataset.label = heads[i] ?? ''));
});

// command palette: Ctrl+K (anywhere) or : opens it. Commands: the nav (g+letter), every visible [data-key] button or link,
// theme, shortcuts help, the app's window.commands = [{label, sub, run}], and a search fallback.
{
  const pal = document.createElement('dialog');
  pal.className = 'palette';
  pal.setAttribute('aria-label', T['palette.title'] || 'commands');
  pal.innerHTML = `<span class="prompt"><input aria-label="${esc(T['palette.title'] || 'commands')}" placeholder="${esc(T['palette.placeholder'] || 'command…')}" autocomplete="off"></span><ul role="listbox"></ul>`;
  document.body.append(pal);
  const inp = $('input', pal), ul = $('ul', pal);
  let cmds = [], hits = [], sel = 0;
  const text = el => [...el.childNodes].filter(n => n.nodeName !== 'KBD').map(n => n.textContent).join('').replace(/\s+/g, ' ').trim();
  const gather = () => {
    const c = [];
    document.querySelectorAll('nav.app a[data-g]').forEach(a => c.push({label: text(a), sub: 'g ' + a.dataset.g, run: () => location = a.href}));
    document.querySelectorAll('header.app .right a[aria-label]').forEach(a => c.push({label: a.getAttribute('aria-label'), sub: '', run: () => location = a.href})); // settings, logout
    document.querySelectorAll('main [data-key]').forEach(el => { if (el.offsetParent !== null) c.push({label: text(el) || el.getAttribute('aria-label') || el.title, sub: el.dataset.key, run: () => el.click()}); });
    if ($('#theme')) c.push({label: T['palette.theme'] || 'switch theme', sub: '◐', run: () => $('#theme').click()});
    if ($('#help')) c.push({label: T['palette.help'] || 'keyboard shortcuts', sub: '?', run: () => $('#help').showModal()});
    return c.concat(window.commands || []).filter(x => x.label);
  };
  // match: contiguous first (earlier is better), then every letter in order; marks what matched
  const match = (label, q) => {
    const l = label.toLowerCase(), i = l.indexOf(q);
    if (!q) return {score: 0, html: esc(label)};
    if (i >= 0) return {score: i, html: esc(label.slice(0, i)) + '<mark>' + esc(label.slice(i, i + q.length)) + '</mark>' + esc(label.slice(i + q.length))};
    let j = 0, html = '';
    for (const ch of label) { if (j < q.length && ch.toLowerCase() === q[j]) { html += '<mark>' + esc(ch) + '</mark>'; j++; } else html += esc(ch); }
    return j === q.length ? {score: 100 + label.length, html} : null;
  };
  const render = () => {
    const q = inp.value.trim().toLowerCase();
    hits = cmds.map(c => ({c, m: match(c.label, q)})).filter(h => h.m).sort((a, b) => a.m.score - b.m.score);
    const form = $('form.gsearch');
    if (q && form) hits.push({c: {label: (T['palette.search'] || 'search') + ': ' + inp.value.trim(), sub: '/', run: () => { form.q.value = inp.value.trim(); form.requestSubmit(); }}, m: {html: esc((T['palette.search'] || 'search') + ': ' + inp.value.trim())}});
    sel = Math.min(sel, Math.max(hits.length - 1, 0));
    ul.innerHTML = hits.map((h, i) => `<li role="option" data-i="${i}"${i === sel ? ' class="sel" aria-selected="true"' : ''}><span>${h.m.html}</span><span class="sub">${esc(h.c.sub || '')}</span></li>`).join('')
      || `<li class="none">${esc(T['search.none'] || '—')}</li>`;
    ul.querySelector('.sel')?.scrollIntoView({block: 'nearest'});
  };
  const run = i => { const h = hits[i]; if (!h) return; pal.close(); h.c.run(); };
  const pb = $('#palbtn');
  if (pb) { pb.hidden = false; pb.onclick = () => window.openPalette(); if (/Mac|iPhone|iPad/.test(navigator.platform)) $('kbd', pb).textContent = '⌘K'; }
  window.openPalette = () => { if (pal.open) return; cmds = gather(); inp.value = ''; sel = 0; render(); pal.showModal(); inp.focus(); };
  inp.addEventListener('input', () => { sel = 0; render(); });
  inp.addEventListener('keydown', e => {
    const down = e.key === 'ArrowDown' || (e.ctrlKey && e.key === 'j'), up = e.key === 'ArrowUp' || (e.ctrlKey && e.key === 'k');
    if (down || up) { e.preventDefault(); sel = (sel + (down ? 1 : hits.length - 1)) % Math.max(hits.length, 1); render(); }
    else if (e.key === 'Enter') { e.preventDefault(); run(sel); }
  });
  ul.addEventListener('click', e => { const li = e.target.closest('li[data-i]'); if (li) run(+li.dataset.i); });
  pal.addEventListener('click', e => { if (e.target === pal) pal.close(); }); // backdrop
  document.addEventListener('keydown', e => {
    const field = e.target.matches('input, select, textarea, [contenteditable]');
    if (((e.ctrlKey || e.metaKey) && e.key === 'k' && !(field && e.target.closest('form.gsearch'))) || (e.key === ':' && !field && !document.querySelector('dialog[open]'))) {
      e.preventDefault(); e.stopImmediatePropagation(); window.openPalette();
    }
  }, true);
}

// chart tooltips: any [data-tip] inside .chart shows its text on hover and keyboard focus (the table view has the same values)
{
  let tip;
  const show = e => {
    const el = e.target.closest?.('.chart [data-tip]');
    if (!el) { tip?.remove(); tip = null; return; }
    tip ??= document.body.appendChild(Object.assign(document.createElement('div'), {className: 'tip'}));
    tip.textContent = el.dataset.tip;
    const r = el.getBoundingClientRect();
    tip.style.left = Math.max(8, Math.min(innerWidth - tip.offsetWidth - 8, r.left + r.width / 2 - tip.offsetWidth / 2)) + 'px';
    tip.style.top = Math.max(8, r.top - tip.offsetHeight - 6) + 'px';
  };
  document.addEventListener('pointerover', show);
  document.addEventListener('focusin', show);
  document.addEventListener('focusout', () => { tip?.remove(); tip = null; });
}

// bulk: td.pick checkboxes belong to <form id="bulk" class="bulkbar"> (form="bulk"); th.pick input[data-all] selects all;
// x toggles the cursor row; the bar shows "{n} selected" and hides while nothing is; data-confirm may say {n}
{
  const bar = document.querySelector('form.bulkbar');
  if (bar) {
    const boxes = () => [...document.querySelectorAll(`td.pick input[form="${bar.id}"]`)];
    const sync = () => {
      const n = boxes().filter(b => b.checked).length, all = $('th.pick input[data-all]');
      bar.hidden = !n;
      $('.n', bar).textContent = (T['js.selected'] || '{n} selected').replace('{n}', n);
      bar.querySelectorAll('[data-confirm-n]').forEach(b => b.dataset.confirm = b.dataset.confirmN.replace('{n}', n));
      if (all) { all.checked = n && n === boxes().length; all.indeterminate = n > 0 && n < boxes().length; }
    };
    document.addEventListener('change', e => {
      if (e.target.matches('th.pick input[data-all]')) boxes().forEach(b => b.checked = e.target.checked);
      if (e.target.closest('td.pick, th.pick')) sync();
    });
    window.pageKeys = Object.assign({x: () => { const b = $('tr.sel td.pick input'); if (!b) return false; b.checked = !b.checked; sync(); }}, window.pageKeys || {});
    sync();
  }
}

// session: <body data-session-end="RFC 3339" data-session-renew="url">: 5 minutes before the end a bar offers to stay signed in
// (GET renew, answer {end}); after the end it says so and keeps the page, so nothing typed is lost; autosave answers 401 the same way
window.sessionCheck = () => {
  const b = document.body, end = Date.parse(b.dataset.sessionEnd || ''), left = Math.round((end - Date.now()) / 60000);
  let bar = $('.session');
  if (Number.isNaN(end) || left > 5) { bar?.remove(); return; } // no session info, or more than 5 minutes left
  if (!bar) { bar = document.createElement('div'); bar.className = 'session'; bar.setAttribute('role', 'alert'); document.body.prepend(bar); }
  const renew = b.dataset.sessionRenew;
  bar.classList.toggle('ended', left <= 0);
  bar.innerHTML = left > 0
    ? `<span>${esc((T['js.session.soon'] || 'Your session ends in {n} min.').replace('{n}', left))}</span>` + (renew ? `<button type="button" class="pri">${esc(T['js.session.renew'] || 'Stay signed in')}</button>` : '')
    : `<span>${esc(T['js.session.ended'] || 'Your session has ended. This page keeps your input; sign in again to save.')}</span>` + (renew ? `<a class="btn pri" href="${esc(renew)}" target="_blank" rel="noopener">${esc(T['js.session.login'] || 'Sign in')}</a>` : '');
  $('button', bar)?.addEventListener('click', async () => {
    const r = await fetch(renew, {headers: {Accept: 'application/json'}}).catch(() => null), j = await r?.json().catch(() => ({})) || {};
    if (r?.ok && j.end) { b.dataset.sessionEnd = j.end; window.sessionCheck(); }
  });
};
window.sessionCheck();
setInterval(window.sessionCheck, 30000);

// dialogs: [data-open=id] opens, [data-close] closes (Escape is native); [data-copy=text] copies; .nojs hides fallbacks.
// An open dialog is part of the URL (#new): a link to /customers#new opens it, closing removes the hash.
const openDlg = id => { const d = document.getElementById(id); if (d?.tagName !== 'DIALOG' || d.open) return; d.showModal(); history.replaceState(history.state, '', '#' + id); };
document.addEventListener('close', e => { if (location.hash === '#' + e.target.id) history.replaceState(history.state, '', location.pathname + location.search); }, true);
if (location.hash.length > 1) openDlg(decodeURIComponent(location.hash.slice(1)));
// prefill: <form data-prefill> takes ?name=…&email=… for its empty fields (the server should render them too; this covers dialogs and static pages)
document.querySelectorAll('form[data-prefill]').forEach(f => { for (const [k, v] of new URLSearchParams(location.search)) { const el = f.elements[k]; if (el && 'value' in el && !el.value && el.type !== 'hidden') el.value = v; } });
document.addEventListener('click', async e => {
  const o = e.target.closest('[data-open]'), c = e.target.closest('[data-copy]');
  if (o) { e.preventDefault(); openDlg(o.dataset.open); }
  else if (e.target.closest('[data-palette]')) window.openPalette();
  else if (e.target.closest('[data-close]')) e.target.closest('dialog').close();
  else if (c) { await navigator.clipboard.writeText(c.dataset.copy).catch(() => {}); c.classList.add('done'); setTimeout(() => c.classList.remove('done'), 1200); } // the icon turns into a check
});
document.querySelectorAll('.nojs').forEach(b => b.hidden = true);

// morph patches a into b in place, so focus and caret survive a live re-render; unfocused inputs take the server value
window.morph = (a, b) => {
  if (a.nodeName !== b.nodeName) return a.replaceWith(b);
  if (a.nodeType !== 1) { if (a.nodeValue !== b.nodeValue) a.nodeValue = b.nodeValue; return; }
  for (const {name} of [...a.attributes]) if (!b.hasAttribute(name)) a.removeAttribute(name);
  for (const {name, value} of b.attributes) if (a.getAttribute(name) !== value) a.setAttribute(name, value);
  if (/^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName) && a !== document.activeElement) {
    if (a.type === 'checkbox' || a.type === 'radio') a.checked = b.checked; else a.value = b.value;
  }
  const ac = [...a.childNodes], bc = [...b.childNodes];
  bc.forEach((n, i) => ac[i] ? morph(ac[i], n) : a.append(n));
  ac.slice(bc.length).forEach(n => n.remove());
};

// toasts: the page's flash messages float bottom right (full width on phones); successes fade after 5s, errors stay until closed.
// Without JS they stay where the page put them.
{
  const ps = document.querySelectorAll('main .flash:is(p, div)');
  if (ps.length) {
    const box = document.createElement('div');
    box.className = 'toasts';
    box.setAttribute('role', 'status');
    document.body.append(box);
    for (const p of ps) {
      const x = document.createElement('button');
      x.type = 'button'; x.className = 'x'; x.textContent = '×'; x.setAttribute('aria-label', T['js.close'] || 'close');
      x.onclick = () => p.remove();
      p.append(x);
      box.append(p);
      if (p.classList.contains('ok')) setTimeout(() => { p.classList.add('out'); setTimeout(() => p.remove(), 400); }, 5000);
    }
  }
}

// back/forward always show current data: a page answering a POST becomes a plain entry (no "resubmit?"),
// and a page restored from the back/forward cache reloads (also clears spinners)
history.replaceState(history.state, '', location.href);
addEventListener('pageshow', e => { if (e.persisted) location.reload(); });
