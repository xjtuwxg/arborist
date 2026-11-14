'use strict';

const $ = (sel) => document.querySelector(sel);

// h builds a DOM node. All text goes through text nodes, never innerHTML:
// file names and model output are untrusted.
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (v !== false && v != null) el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat(3)) {
    if (kid == null || kid === false) continue;
    el.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

// fill replaces a node's children; nested arrays and null are allowed.
function fill(el, ...kids) {
  el.replaceChildren(...kids.flat(3).filter((k) => k != null && k !== false)
    .map((k) => (k.nodeType ? k : document.createTextNode(String(k)))));
}

// Icons are fixed strings from this file, so innerHTML is safe here.
const ICONS = {
  folder: '<path d="M3 6.5A2.5 2.5 0 0 1 5.5 4h3.6c.6 0 1.2.3 1.6.8l.9 1.2h6.9A2.5 2.5 0 0 1 21 8.5v9A2.5 2.5 0 0 1 18.5 20h-13A2.5 2.5 0 0 1 3 17.500z"/>',
  file: '<path d="M7 3h7l5 5v11a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2z"/><path d="M14 3v5h5"/>',
  history: '<path d="M3 12a9 9 0 1 0 3-6.7"/><path d="M3 4v4h4"/><path d="M12 8v4l3 2"/>',
  trash: '<path d="M4 7h16"/><path d="M9 7V4h6v3"/><path d="M6 7l1 13h10l1-13"/>',
  scan: '<circle cx="11" cy="11" r="6.5"/><path d="m16 16 4.500 4.500"/>',
  spark: '<path d="M12 3l1.800 5.200L19 10l-5.200 1.800L12 17l-1.800-5.200L5 10l5.200-1.800z"/><path d="M19 16l.7 2 2 .7-2 .7-.7 2-.7-2-2-.7 2-.7z"/>',
  split: '<rect x="3" y="4" width="18" height="16" rx="2.500"/><path d="M12 4v16"/>',
};
function icon(name, cls) {
  const span = h('span', { class: 'ico ' + (cls || name), 'aria-hidden': 'true' });
  span.innerHTML = `<svg viewBox="0 0 24 24">${ICONS[name]}</svg>`;
  return span;
}
for (const el of document.querySelectorAll('[data-icon]')) el.replaceWith(icon(el.dataset.icon));

async function api(path, body) {
  const opt = body === undefined ? {} : {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Arborist': '1' },
    body: JSON.stringify(body),
  };
  const res = await fetch(path, opt);
  const text = await res.text();
  let data;
  try { data = JSON.parse(text); } catch { data = { error: text.trim() }; }
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

const store = {
  get(k) { try { return localStorage.getItem(k); } catch { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* private window */ } },
};

const state = { root: '', path: '', view: 'files', provider: null, providers: [], listing: null, selected: null, tab: 'analysis' };

function fmtSize(n) {
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(n < 10 ? 1 : 0) + ' ' + units[i];
}
const fmtDate = (s) => (s && !s.startsWith('0001') ? s.slice(0, 10) : '');
const fmtTime = (s) => s.slice(0, 16).replace('T', ' ');
const num = (n) => h('span', { class: n ? '' : 'zero' }, n ? n.toLocaleString() : '0');
const join = (a, b) => (a ? a + '/' + b : b);
const plural = (n, word, many) => `${n.toLocaleString()} ${n === 1 ? word : many || word + 's'}`;
const rootName = () => state.root.split('/').pop() || '/';

function toast(msg, opt = {}) {
  const el = h('div', { class: 'toast' + (opt.bad ? ' bad' : '') }, h('span', {}, msg),
    opt.action ? h('button', { onclick: () => { el.remove(); opt.action.run(); } }, opt.action.label) : null);
  $('#toasts').append(el);
  setTimeout(() => el.remove(), opt.action ? 12000 : 6000);
}

// ---- AI provider and model ------------------------------------------------

const current = () => state.providers.find((p) => p.id === state.provider);
const modelKey = () => 'arborist.model.' + state.provider;
const OTHER = '\u0000other';
const modelValue = () => ($('#model').value === OTHER ? $('#model-custom').value.trim() : $('#model').value);

// usable: signed in as far as we can tell. A failed sign-in check overrides
// the presence of a credential file.
const usable = (p) => p.ready && !(p.check && !p.check.ok);
// reasonOf picks the line of a tool's error output that says what went wrong;
// the rest is usually warnings.
function reasonOf(s) {
  const lines = String(s || '').split('\n').map((l) => l.trim()).filter((l) => l && !/^at /.test(l));
  const hit = lines.filter((l) => /error|expired|authenticat|unauthori|not supported|no longer supported|unsupported|log ?in|sign ?in|quota|limit|did not answer/i.test(l)).pop() || lines.pop() || '';
  return hit.length > 140 ? hit.slice(0, 140) + '…' : hit;
}

function pickProvider() {
  const ready = state.providers.filter(usable);
  const saved = store.get('arborist.provider');
  state.provider = (ready.find((p) => p.id === saved) || ready[0] || {}).id || null;
}

function renderAI() {
  const sel = $('#provider');
  fill(sel, state.providers.map((p) => h('option', { value: p.id, disabled: !usable(p), selected: p.id === state.provider },
    p.name + (usable(p) ? '' : p.ready ? ' (not answering)' : ' (not signed in)'))));
  if (!state.providers.length) fill(sel, h('option', { value: '' }, 'None found'));
  sel.disabled = !state.provider;
  const p = current();
  const model = $('#model');
  const saved = p ? store.get(modelKey()) || '' : '';
  const listed = (p && p.models) || [];
  const other = saved !== '' && !listed.includes(saved);
  model.disabled = !p;
  const chk = (p && p.check) || {};
  const answered = chk.models || {};
  const failed = chk.failed || {};
  const def = answered[''] || (p && p.default);
  fill(model, [
    h('option', { value: '', selected: saved === '' }, def ? `Default (${def})` : 'Default'),
    ...listed.map((m) => h('option', { value: m, selected: m === saved, disabled: m in failed, title: m in failed ? reasonOf(failed[m]) : '' },
      m + (answered[m] ? ` · ${answered[m]}` : m in failed ? ' (unavailable)' : ''))),
    h('option', { value: OTHER, selected: other }, 'Other…'),
  ]);
  $('#model-other').hidden = !other;
  $('#model-custom').value = other ? saved : '';
  const lines = [];
  if (p) {
    const when = p.check && p.check.ok ? `, checked ${ago(p.check.at)}` : state.checking ? ', checking the sign-in…' : ', sign-in not checked';
    lines.push(h('div', {}, `Using your ${p.name} sign-in on this machine` + (p.version ? ` (${p.version.replace(/\s*\(.*\)/, '')})` : '') + when + '.'));
  } else if (state.providers.length) {
    lines.push(h('div', {}, 'AI tools are installed but none is signed in.'));
  } else {
    lines.push(h('div', {}, 'No AI tool found. Browsing and reference scans still work.'));
  }
  for (const q of state.providers.filter((q) => !usable(q))) {
    const why = q.ready ? reasonOf(Object.values(q.check.failed || {})[0]) : q.note;
    lines.push(h('div', { class: 'bad' }, `${q.name} is ${q.ready ? 'not answering' : 'not signed in'}.`,
      why ? h('span', { class: 'why' }, ' ' + why) : null,
      ...(q.login && (!q.ready || /authenticat|unauthori|expired|credential|log ?in|sign ?in/i.test(why))
        ? [h('span', {}, ' To sign in again, run in a terminal here:'), h('code', {}, q.login)] : [])));
  }
  fill($('#ai-hint'), lines);
  renderActions();
}

$('#provider').addEventListener('change', (ev) => {
  state.provider = ev.target.value;
  store.set('arborist.provider', state.provider);
  renderAI();
});
$('#model').addEventListener('change', (ev) => {
  const other = ev.target.value === OTHER;
  $('#model-other').hidden = !other;
  if (other) $('#model-custom').focus();
  store.set(modelKey(), modelValue());
});
$('#model-custom').addEventListener('change', () => store.set(modelKey(), modelValue()));
$('#btn-rescan-ai').addEventListener('click', async (ev) => {
  const btn = ev.currentTarget;
  btn.disabled = true;
  btn.textContent = 'Checking sign-ins…';
  try {
    state.providers = await api('/api/providers/check', {});
    const keep = current();
    if (!keep || !usable(keep)) pickProvider();
    const bad = state.providers.filter((p) => p.ready && !usable(p));
    toast(`${plural(state.providers.filter(usable).length, 'AI tool')} signed in and answering.` + (bad.length ? ` Not answering: ${bad.map((p) => p.name).join(', ')}.` : ''), { bad: bad.length > 0 });
  } catch (err) { toast(err.message, { bad: true }); }
  btn.disabled = false;
  btn.textContent = 'Check sign-ins';
  renderAI();
});

// The server checks sign-ins in the background when it starts; pick the result up.
async function awaitChecks() {
  while (state.checking) {
    await new Promise((r) => setTimeout(r, 3000));
    let s;
    try { s = await api('/api/state'); } catch { return; }
    state.providers = s.providers || [];
    state.checking = !!s.checking;
    const keep = current();
    if (!keep || !usable(keep)) pickProvider();
    renderAI();
  }
}

function renderWelcome() {
  const box = $('#welcome');
  if (store.get('arborist.welcomed')) return;
  const ready = state.providers.filter(usable);
  if (!ready.length) return;
  fill(box, h('p', {}, `Found ${plural(ready.length, 'AI tool')} already signed in on this machine: ` +
    ready.map((p) => p.name).join(', ') + `. Arborist will use ${current().name}; change the provider or model in the sidebar. ` +
    'Analysis sends folder names and the first lines of README files to that tool. Nothing moves until you confirm a preview, and nothing is ever deleted without a second confirmation.'),
  h('button', { onclick: () => { box.hidden = true; store.set('arborist.welcomed', '1'); } }, 'Got it'));
  box.hidden = false;
}

// ---- views and navigation -------------------------------------------------

function showView(view) {
  state.view = view;
  for (const b of document.querySelectorAll('#nav button')) {
    if (b.dataset.view === view) b.setAttribute('aria-current', 'page'); else b.removeAttribute('aria-current');
  }
  for (const v of ['files', 'history', 'trash']) $('#view-' + v).hidden = v !== view;
  $('#actions').hidden = view !== 'files';
  renderCrumbs();
  if (view === 'history') loadHistory();
  if (view === 'trash') loadTrash();
}
for (const b of document.querySelectorAll('#nav button')) b.addEventListener('click', () => showView(b.dataset.view));

function renderCrumbs() {
  const box = $('#crumbs');
  if (state.view !== 'files') {
    fill(box, h('strong', {}, state.view === 'history' ? 'History' : 'Trash'));
    return;
  }
  const parts = state.path ? state.path.split('/') : [];
  const kids = [parts.length ? h('a', { onclick: () => go('') }, rootName()) : h('strong', {}, rootName())];
  parts.forEach((part, i) => {
    kids.push(h('span', { class: 'sep' }, '/'));
    const target = parts.slice(0, i + 1).join('/');
    kids.push(i === parts.length - 1 ? h('strong', {}, part) : h('a', { onclick: () => go(target) }, part));
  });
  fill(box, kids);
}

function renderActions() {
  const a = state.listing && state.listing.analysis;
  $('#btn-analyze').disabled = !state.provider;
  $('#btn-analyze').title = state.provider ? 'Describe each entry and propose a reorganization' : 'No signed-in AI tool found';
  $('#btn-analyze').lastChild.textContent = a ? 'Analyze again' : 'Analyze';
  $('#btn-scan').lastChild.textContent = state.listing && state.listing.scanned ? 'Rescan references' : 'Scan references';
  const ig = $('#btn-ignore');
  ig.hidden = !state.path;
  ig.textContent = state.listing && state.listing.ignored ? 'Include in scans' : 'Exclude from scans';
  ig.title = 'Scans skip excluded folders. The list is the .arboristignore file in the workspace root.';
  $('#btn-scan').disabled = !!(state.listing && state.listing.ignored);
  $('#btn-dry').disabled = !(a && a.proposals && a.proposals.length);
  $('#btn-dry').title = $('#btn-dry').disabled ? 'Analyze this folder first' : 'Preview the reorganized folder next to the current one. Nothing moves until you confirm.';
}

async function go(path) {
  state.path = path;
  state.selected = null;
  const hash = '#' + encodeURIComponent(path);
  if (location.hash !== hash && !(path === '' && location.hash === '')) history.pushState(null, '', hash);
  if (state.view !== 'files') showView('files');
  await reload();
}

async function reload() {
  try {
    state.listing = await api('/api/ls?path=' + encodeURIComponent(state.path));
  } catch (err) {
    state.listing = { entries: [], error: err.message };
  }
  renderCrumbs();
  renderActions();
  renderFiles();
  renderAnalysis();
  if (state.tab === 'refs') loadRefs();
  refreshTrashCount();
}

async function refreshTrashCount() {
  try {
    const items = await api('/api/trash');
    $('#trash-count').textContent = items.length;
    $('#trash-count').hidden = !items.length;
  } catch { /* shown elsewhere */ }
}

// ---- file table -----------------------------------------------------------

function renderFiles() {
  const body = $('#files tbody');
  const { entries, analysis, error } = state.listing;
  const items = new Map(((analysis && analysis.items) || []).map((it) => [it.name, it]));
  $('#files').classList.toggle('plain', items.size === 0);
  const empty = $('#files-empty');
  empty.hidden = entries.length > 0;
  empty.textContent = error || 'This folder is empty.';
  fill(body, entries.map((e) => {
    const it = items.get(e.name);
    const open = () => (e.dir ? go(join(state.path, e.name)) : preview(e.name));
    return h('tr', { class: state.selected === e.name ? 'selected' : '', tabindex: 0, onclick: open,
      onkeydown: (ev) => { if (ev.key === 'Enter') open(); } },
    h('td', { class: 'name', title: e.name }, icon(e.dir ? 'folder' : 'file'), e.name,
      e.link ? h('span', { class: 'link' }, ' → ' + e.link) : null),
    h('td', { class: 'what ai-col' }, it ? it.what : ''),
    h('td', { class: 'ai-col' }, it && it.status && it.status !== 'unknown' ? h('span', { class: 'tag ' + it.status }, it.status) : ''),
    h('td', { class: 'num' }, e.dir ? '' : fmtSize(e.size)),
    h('td', { class: 'when' }, fmtDate(e.mtime)),
    h('td', { class: 'num' }, num(e.refsIn)),
    h('td', { class: 'num' }, num(e.refsOut)));
  }));
}

// ---- inspector ------------------------------------------------------------

function showTab(name) {
  state.tab = name;
  for (const b of document.querySelectorAll('.tabs button')) b.setAttribute('aria-selected', b.dataset.tab === name);
  for (const t of ['analysis', 'refs', 'preview']) $('#tab-' + t).hidden = t !== name;
  if (name === 'refs') loadRefs();
}
for (const b of document.querySelectorAll('.tabs button')) b.addEventListener('click', () => showTab(b.dataset.tab));

function impactNode(im) {
  if (!im) return null;
  if (!im.scanned) return h('span', { class: 'impact unknown' }, 'References not scanned for these entries');
  if (!im.breaks) return h('span', { class: 'impact safe' }, 'No absolute references point here');
  return h('span', { class: 'impact breaks' },
    `Moving breaks ${plural(im.breaks, 'reference')} in ${plural(im.files, 'file')}` +
    (im.authored ? ` (${im.authored.toLocaleString()} hand-written)` : ' (all generated)'));
}

const destOf = (p) => (p.op === 'archive' ? '_archive' : p.op === 'trash' ? 'Trash' : p.to);

function renderAnalysis() {
  const box = $('#tab-analysis');
  const a = state.listing.analysis;
  if (!a) {
    fill(box, h('p', { class: 'muted' }, state.provider
      ? 'This folder has not been analyzed. Analyze describes each entry and proposes how to reorganize it. Nothing moves until you confirm a preview.'
      : 'No signed-in AI tool was found on this machine, so analysis is unavailable.'));
    return;
  }
  const cats = new Map();
  for (const it of a.items || []) cats.set(it.category || 'other', (cats.get(it.category || 'other') || 0) + 1);
  const props = a.proposals || [];
  fill(box,
    h('p', { class: 'lead' }, a.summary || ''),
    h('p', { class: 'muted' }, `${a.provider} · ${fmtTime(a.at)}`),
    h('h3', {}, 'Topics'),
    h('div', { class: 'chips' }, [...cats].sort((x, y) => y[1] - x[1]).map(([c, n]) => h('span', { class: 'tag' }, `${c} · ${n}`))),
    h('h3', {}, `Proposals (${props.length})`),
    props.length ? null : h('p', { class: 'muted' }, 'Nothing left to change here.'),
    props.map((p) => h('div', { class: 'proposal' },
      h('span', { class: 'tag op ' + p.op }, p.op),
      h('div', { class: 'paths' }, p.from.join(', '), h('span', { class: 'muted' }, ' → '), h('b', {}, destOf(p))),
      h('div', { class: 'reason' }, p.reason || ''),
      impactNode(p.impact))));
}

function refNode(r) {
  return h('div', { class: 'ref' },
    r.file + (r.line ? ':' + r.line : ''), h('span', { class: 'arrow' }, ' → '), r.target,
    h('span', { class: 'tag' }, r.category),
    r.cross ? h('span', { class: 'tag' }, 'cross-project') : null,
    r.dangling ? h('span', { class: 'tag dangling' }, 'dangling') : null);
}

async function loadRefs() {
  const box = $('#tab-refs');
  const path = state.path;
  let d;
  try { d = await api('/api/refs?path=' + encodeURIComponent(path)); } catch (err) {
    fill(box, h('p', { class: 'muted' }, err.message));
    return;
  }
  if (path !== state.path) return;
  if (!d.scanned) {
    fill(box, h('p', { class: 'muted' },
      'This folder has not been scanned. A scan finds absolute paths in its files and symlinks that point into the workspace: the things a move would break.'));
    return;
  }
  const more = (shown, total) => (total > shown ? h('p', { class: 'muted' }, `Showing ${shown} of ${total.toLocaleString()}.`) : null);
  fill(box,
    h('h3', {}, `Into this folder from elsewhere (${d.inTotal.toLocaleString()})`),
    (d.in || []).length ? d.in.map(refNode) : h('p', { class: 'muted' }, 'None.'),
    more((d.in || []).length, d.inTotal),
    h('h3', {}, `From files in this folder (${d.outTotal.toLocaleString()})`),
    (d.out || []).length ? d.out.map(refNode) : h('p', { class: 'muted' }, 'None.'),
    more((d.out || []).length, d.outTotal));
}

async function preview(name) {
  state.selected = name;
  renderFiles();
  showTab('preview');
  const box = $('#tab-preview');
  fill(box, h('p', { class: 'muted' }, 'Loading…'));
  try {
    const d = await api('/api/preview?path=' + encodeURIComponent(join(state.path, name)));
    fill(box,
      h('p', { class: 'muted', style: 'margin-bottom:10px' }, `${name} · ${fmtSize(d.size)}` + (d.truncated ? ' · first 64 KB shown' : '')),
      d.binary ? h('p', { class: 'muted' }, 'Binary file, no preview.') : h('pre', { class: 'preview' }, d.text));
  } catch (err) {
    fill(box, h('p', { class: 'muted' }, err.message));
  }
}

// ---- dry run --------------------------------------------------------------

const dry = { selected: null, plan: null, repair: true };

function treeNode(n) {
  const note = n.state === 'leaving' ? '→ ' + n.other
    : n.state === 'new' ? 'new folder'
      : n.state === 'moved' ? 'moved here'
        : n.state === 'trash' && n.name === 'Trash' ? 'kept until you delete it' : '';
  return h('li', {},
    h('div', { class: 'row ' + n.state, title: n.name }, icon(n.state === 'trash' && n.name === 'Trash' ? 'trash' : n.dir ? 'folder' : 'file',
      n.state === 'trash' && n.name === 'Trash' ? 'trash' : undefined),
    h('span', { class: 'n' }, n.name), note ? h('span', { class: 'to' }, note) : null),
    n.children && n.children.length ? h('ul', {}, n.children.map(treeNode)) : null);
}

async function refreshDry() {
  const proposals = state.listing.analysis.proposals;
  let plan;
  try {
    plan = await api('/api/plan', { path: state.path, selected: dry.selected });
  } catch (err) {
    toast(err.message, { bad: true });
    return;
  }
  dry.plan = plan;
  const perStep = new Map();
  for (const m of plan.moves) perStep.set(m.proposal, (perStep.get(m.proposal) || 0) + m.breaks);
  fill($('#dry-steps'), proposals.map((p, i) => h('label', { class: 'step' },
    h('input', { type: 'checkbox', checked: dry.selected.includes(i), onchange: (ev) => {
      dry.selected = ev.target.checked ? [...dry.selected, i] : dry.selected.filter((x) => x !== i);
      refreshDry();
    } }),
    h('div', {},
      h('span', { class: 'tag op ' + p.op }, p.op),
      h('div', { class: 'what' }, p.from.join(', '), ' → ', destOf(p)),
      h('div', { class: 'why' }, p.reason || ''),
      perStep.get(i) ? h('span', { class: 'impact breaks' }, `breaks ${plural(perStep.get(i), 'reference')}`) : null))));
  fill($('#dry-before'), h('ul', {}, plan.before.map(treeNode)));
  fill($('#dry-after'), h('ul', {}, plan.after.map(treeNode)));
  const trashed = plan.moves.filter((m) => m.op === 'trash').length;
  const lines = [h('div', { class: 'big' }, `${plural(plan.moves.length, 'entry', 'entries')} will move. Nothing is deleted or overwritten.`)];
  if (trashed) lines.push(h('div', { class: 'muted' }, `${plural(trashed, 'entry', 'entries')} ${trashed === 1 ? 'goes' : 'go'} to the Trash and ${trashed === 1 ? 'stays' : 'stay'} there until you delete ${trashed === 1 ? 'it' : 'them'} yourself.`));
  if (plan.breaks) {
    const left = plan.breaks - plan.repairs;
    lines.push(h('label', { class: 'check' },
      h('input', { type: 'checkbox', checked: dry.repair, onchange: (ev) => { dry.repair = ev.target.checked; refreshDry(); } }),
      `Rewrite references to the new paths (${plural(plan.repairs, 'reference')} in ${plural(plan.files, 'file')})`));
    if (dry.repair) {
      lines.push(h('div', { class: 'impact safe' },
        `${plural(plan.breaks, 'absolute reference')} ${plan.breaks === 1 ? 'points' : 'point'} at these entries. ${plan.repairs.toLocaleString()} will be rewritten; each edited file is backed up and Undo restores it.`));
      if (left) lines.push(h('div', { class: 'impact unknown' }, `${plural(left, 'reference')} in log files will keep the old path.`));
    } else {
      lines.push(h('div', { class: 'impact breaks' }, `${plural(plan.breaks, 'absolute reference')} will stop resolving until you undo.`));
    }
    if (plan.unscanned) lines.push(h('div', { class: 'impact unknown' }, 'Some entries have no reference scan; references to them are unknown and will not be repaired.'));
  } else if (plan.unscanned) {
    lines.push(h('div', { class: 'impact unknown' }, 'Some entries have no reference scan, so breakage is unknown. Scan references first to see it.'));
  } else if (plan.moves.length) {
    lines.push(h('div', { class: 'impact safe' }, 'No scanned absolute reference points at these entries.'));
  }
  for (const c of plan.conflicts) lines.push(h('div', { class: 'conflict' }, c));
  fill($('#dry-summary'), lines);
  const btn = $('#dry-apply');
  btn.disabled = plan.conflicts.length > 0 || plan.moves.length === 0;
  btn.textContent = plan.conflicts.length ? 'Resolve conflicts first' : `Move ${plural(plan.moves.length, 'entry', 'entries')}`;
}

async function openDry() {
  dry.selected = state.listing.analysis.proposals.map((_, i) => i);
  $('#dry-sub').textContent = `${state.path || rootName()} · nothing has moved yet. Untick the steps you do not want, then confirm.`;
  $('#dry').hidden = false;
  await refreshDry();
  $('#dry-apply').focus();
}
const closeDry = () => { $('#dry').hidden = true; };

$('#btn-dry').addEventListener('click', openDry);
$('#dry-close').addEventListener('click', closeDry);
$('#dry-cancel').addEventListener('click', closeDry);
$('#dry').addEventListener('click', (ev) => { if (ev.target.id === 'dry') closeDry(); });
document.addEventListener('keydown', (ev) => { if (ev.key === 'Escape' && !$('#dry').hidden) closeDry(); });
$('#dry-apply').addEventListener('click', async () => {
  const btn = $('#dry-apply');
  btn.disabled = true;
  try {
    const tx = await api('/api/apply', { path: state.path, selected: dry.selected, repair: dry.repair });
    closeDry();
    const fixed = (tx.edits || []).reduce((n, e) => n + e.count, 0);
    if ((tx.problems || []).length) toast(`${plural(tx.problems.length, 'file')} could not be repaired. See History.`, { bad: true });
    toast(`Moved ${plural(tx.moves.length, 'entry', 'entries')}` + (fixed ? ` and rewrote ${plural(fixed, 'reference')}.` : '.'), { action: { label: 'Undo', run: () => undo(tx.id) } });
  } catch (err) {
    toast(err.message, { bad: true });
    await refreshDry();
  }
  await reload();
});

async function undo(id) {
  try {
    await api('/api/undo', { id });
    toast('Undone. Everything is back where it was.');
  } catch (err) {
    toast(err.message, { bad: true });
  }
  await reload();
  if (state.view === 'history') loadHistory();
}

// ---- history and trash ----------------------------------------------------

async function loadHistory() {
  const box = $('#view-history');
  let d;
  try { d = await api('/api/history'); } catch (err) { fill(box, h('p', { class: 'muted' }, err.message)); return; }
  fill(box,
    h('div', { class: 'page-title' }, 'History'),
    h('p', { class: 'page-sub' }, 'Every applied plan is journaled. The most recent one can be undone, which puts each entry back at its old path.'),
    d.txs.length ? null : h('p', { class: 'muted' }, 'Nothing has been applied yet.'),
    d.txs.map((tx) => h('div', { class: 'card' },
      h('div', { class: 'head' },
        h('span', { class: 'title' }, `${plural(tx.moves.length, 'move')} in ${tx.path || rootName()}`),
        h('span', { class: 'tag ' + (tx.status === 'applied' ? 'active' : tx.status === 'failed' ? 'dead' : '') }, tx.status),
        h('span', { class: 'muted' }, fmtTime(tx.at)),
        h('span', { class: 'spacer' }),
        tx.id === d.undoable ? h('button', { onclick: () => undo(tx.id) }, 'Undo') : null),
      tx.error ? h('div', { class: 'conflict' }, tx.error) : null,
      (tx.edits || []).length ? h('p', { class: 'muted', style: 'margin-top:8px' },
        `References rewritten in ${plural(tx.edits.length, 'file')}: ` + tx.edits.slice(0, 6).map((e) => e.file).join(', ') + (tx.edits.length > 6 ? ', …' : '')) : null,
      (tx.problems || []).map((p) => h('div', { class: 'conflict' }, 'Not repaired: ' + p)),
      h('div', { class: 'moves' }, tx.moves.map((m) => h('div', {},
        h('span', { class: 'tag op ' + m.op }, m.op), ' ', m.from, h('span', { class: 'muted' }, ' → '), m.to,
        m.purged ? h('span', { class: 'tag dead', style: 'margin-left:6px' }, 'deleted') : null,
        m.restored ? h('span', { class: 'tag', style: 'margin-left:6px' }, 'restored') : null))))));
}

async function loadTrash() {
  const box = $('#view-trash');
  let items;
  try { items = await api('/api/trash'); } catch (err) { fill(box, h('p', { class: 'muted' }, err.message)); return; }
  $('#trash-count').textContent = items.length;
  $('#trash-count').hidden = !items.length;
  fill(box,
    h('div', { class: 'page-title' }, 'Trash'),
    h('p', { class: 'page-sub' }, 'Arborist never deletes on its own. Entries it wanted to remove wait here, intact, in the _trash folder of the workspace. Restore puts one back; deleting is permanent and asks you to type the name.'),
    items.length ? null : h('p', { class: 'muted' }, 'The trash is empty.'),
    items.map((it) => {
      const name = it.from.split('/').pop();
      const confirm = h('div', { class: 'confirm', hidden: true });
      const input = h('input', { placeholder: name, 'aria-label': `Type ${name} to confirm`, autocomplete: 'off', spellcheck: 'false' });
      const really = h('button', { class: 'danger solid', disabled: true, onclick: async () => {
        try {
          await api('/api/trash/delete', { tx: it.tx, index: it.index, confirm: input.value });
          toast(`${name} was deleted permanently.`);
        } catch (err) { toast(err.message, { bad: true }); }
        loadTrash();
      } }, 'Delete permanently');
      input.addEventListener('input', () => { really.disabled = input.value !== name; });
      fill(confirm, h('span', {}, 'This cannot be undone. Type the name to confirm:'), input, really,
        h('button', { class: 'ghost', onclick: () => { confirm.hidden = true; } }, 'Cancel'));
      return h('div', { class: 'card' },
        h('div', { class: 'head' },
          icon(it.dir ? 'folder' : 'file'),
          h('span', { class: 'title' }, it.from),
          h('span', { class: 'muted' }, 'trashed ' + fmtTime(it.at)),
          h('span', { class: 'spacer' }),
          h('button', { onclick: async () => {
            try { await api('/api/trash/restore', { tx: it.tx, index: it.index }); toast(`${name} is back at ${it.from}.`); }
            catch (err) { toast(err.message, { bad: true }); }
            loadTrash();
          } }, 'Restore'),
          h('button', { class: 'danger', onclick: () => { confirm.hidden = false; input.focus(); } }, 'Delete…')),
        it.why ? h('p', { class: 'muted', style: 'margin-top:6px' }, it.why) : null,
        confirm);
    }));
}

// ---- jobs -----------------------------------------------------------------

const count = (n) => (n >= 1e6 ? (n / 1e6).toFixed(1) + 'M' : n >= 1e4 ? Math.round(n / 1e3) + 'k' : String(n));
const span = (s) => (s < 90 ? `${Math.round(s)}s` : s < 5400 ? `${Math.round(s / 60)} min` : `${(s / 3600).toFixed(1)} h`);
const ago = (t) => { const s = (Date.now() - new Date(t)) / 1000; return s < 90 ? 'just now' : span(s) + ' ago'; };
let pollTimer = null;
const finished = new Set();
let primed = false;

async function pollJobs() {
  let jobs;
  try { jobs = (await api('/api/jobs')) || []; } catch { return; }
  let running = false;
  let changed = false;
  const nodes = [];
  for (const j of jobs.slice(-3).reverse()) {
    const where = j.path || rootName();
    let text;
    let bar = null;
    if (j.status === 'running') {
      running = true;
      const secs = Math.round((Date.now() - new Date(j.started)) / 1000);
      text = `${j.kind === 'scan' ? 'Scanning' : 'Analyzing'} ${where}… ${span(secs)}`;
      if (j.kind === 'scan' && j.total > 0) {
        // The total is what the last scan saw, so it is an estimate; never show 100% while running.
        const frac = Math.min(0.99, (j.seen || 0) / j.total);
        const left = frac > 0.02 ? ` · about ${span(secs * (1 - frac) / frac)} left` : '';
        text += ` · ${count(j.seen || 0)} of about ${count(j.total)} files · ${Math.floor(frac * 100)}%${left}`;
        bar = h('progress', { max: 1, value: frac });
      } else if (j.kind === 'scan') {
        text += ` · ${count(j.seen || 0)} files so far. First scan of this folder, so the total is not known yet.`;
        bar = h('progress', {});
      } else if (j.detail) {
        text += ` · ${j.detail}`;
      }
    } else if (j.status === 'failed') {
      text = `${j.kind === 'scan' ? 'Scan' : 'Analysis'} of ${where} failed: ${j.error}`;
    } else {
      text = `${j.kind === 'scan' ? 'Scan' : 'Analysis'} of ${where}: ${j.detail}`;
    }
    if (j.status !== 'running' && !finished.has(j.id)) { finished.add(j.id); changed = true; }
    nodes.push(h('span', { class: 'job ' + j.status }, bar, text));
  }
  fill($('#jobs'), nodes);
  if (changed && primed) await reload();
  primed = true;
  clearTimeout(pollTimer);
  if (running) pollTimer = setTimeout(pollJobs, 1500);
}

async function startJob(kind, body) {
  try {
    await api('/api/' + kind, body);
  } catch (err) {
    toast(err.message, { bad: true });
    return;
  }
  pollJobs();
}

$('#btn-scan').addEventListener('click', () => startJob('scan', { path: state.path }));
$('#btn-ignore').addEventListener('click', async () => {
  const ignore = !state.listing.ignored;
  try {
    const d = await api('/api/ignore', { path: state.path, ignore });
    toast(d.ignored ? `${state.path} will be skipped by scans from now on.`
      : ignore ? `${state.path} is excluded.` : `${state.path} will be scanned again.`);
  } catch (err) { toast(err.message, { bad: true }); }
  await reload();
});
$('#btn-analyze').addEventListener('click', () => {
  showTab('analysis');
  startJob('analyze', { path: state.path, provider: state.provider, model: modelValue() });
});

// ---- start ----------------------------------------------------------------

const fromHash = () => decodeURIComponent(location.hash.slice(1));
window.addEventListener('popstate', () => { if (fromHash() !== state.path) go(fromHash()); });

(async function init() {
  const s = await api('/api/state');
  state.root = s.root;
  state.providers = s.providers || [];
  state.checking = !!s.checking;
  awaitChecks();
  fill($('#machine'), s.host, h('br'), s.root);
  document.title = `Arborist · ${s.host}`;
  pickProvider();
  renderAI();
  renderWelcome();
  await go(fromHash());
  pollJobs();
})();
