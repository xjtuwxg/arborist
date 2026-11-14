'use strict';

const $ = (sel) => document.querySelector(sel);

// h builds a DOM node. All text goes through textContent, never innerHTML:
// file names and model output are untrusted.
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (v !== false && v != null) el.setAttribute(k, v);
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    el.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

// fill replaces a node's children; nested arrays and null are allowed.
function fill(el, ...kids) {
  el.replaceChildren(...kids.flat(2).filter((k) => k != null && k !== false));
}

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

const state = { root: '', path: '', provider: null, providers: [], listing: null, selected: null, tab: 'analysis' };

function fmtSize(n) {
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(n < 10 ? 1 : 0) + ' ' + units[i];
}
const fmtDate = (s) => (s && !s.startsWith('0001') ? s.slice(0, 10) : '');
const num = (n) => h('span', { class: n ? '' : 'zero' }, n ? n.toLocaleString() : '0');
const join = (a, b) => (a ? a + '/' + b : b);

// ---- header -------------------------------------------------------------

function renderProviders() {
  const box = $('#providers');
  fill(box);
  if (!state.providers.length) {
    box.append(h('span', { class: 'muted' }, 'No AI tools found on this machine'));
  }
  for (const p of state.providers) {
    const cls = 'chip' + (p.ready ? ' ready' : ' off') + (state.provider === p.id ? ' selected' : '');
    const title = [p.version, p.path, p.note, p.models && p.models.length ? 'models: ' + p.models.join(', ') : '']
      .filter(Boolean).join('\n');
    box.append(h('button', {
      class: cls, title, disabled: !p.ready, 'aria-pressed': state.provider === p.id,
      onclick: () => { state.provider = p.id; store.set('arborist.provider', p.id); renderProviders(); renderActions(); },
    }, p.name));
  }
  box.append(h('button', { title: 'Look for AI tools again', onclick: refreshProviders }, 'Rescan'));
}

async function refreshProviders() {
  state.providers = await api('/api/providers/refresh', {});
  pickProvider();
  renderProviders();
  renderActions();
}

function pickProvider() {
  const ready = state.providers.filter((p) => p.ready);
  const saved = store.get('arborist.provider');
  state.provider = (ready.find((p) => p.id === saved) || ready[0] || {}).id || null;
}

function renderWelcome() {
  const box = $('#welcome');
  if (store.get('arborist.welcomed')) return;
  const ready = state.providers.filter((p) => p.ready);
  const rest = state.providers.filter((p) => !p.ready);
  let msg;
  if (ready.length) {
    const using = ready.find((p) => p.id === state.provider);
    msg = `Found ${ready.length} AI tool${ready.length > 1 ? 's' : ''} already signed in on this machine: ` +
      ready.map((p) => p.name).join(', ') + `. Arborist will use ${using.name}; pick another in the top bar. ` +
      'Analysis sends folder names and the first lines of README files to that tool. Nothing is moved.';
  } else if (rest.length) {
    msg = 'Found ' + rest.map((p) => p.name).join(', ') + ', but none is signed in. Sign in from a terminal, then press Rescan.';
  } else {
    msg = 'No AI command-line tool was found (Claude Code, Codex, Gemini CLI, opencode, Ollama). Browsing and reference scanning still work.';
  }
  fill(box, h('p', {}, msg),
    h('button', { onclick: () => { box.hidden = true; store.set('arborist.welcomed', '1'); } }, 'Got it'));
  box.hidden = false;
}

// ---- navigation ---------------------------------------------------------

function renderCrumbs() {
  const box = $('#crumbs');
  const parts = state.path ? state.path.split('/') : [];
  const rootName = state.root.split('/').pop() || '/';
  fill(box, h('a', { onclick: () => go('') }, rootName));
  parts.forEach((part, i) => {
    box.append(h('span', { class: 'sep' }, '/'));
    const target = parts.slice(0, i + 1).join('/');
    box.append(i === parts.length - 1 ? h('strong', {}, part) : h('a', { onclick: () => go(target) }, part));
  });
}

function renderActions() {
  $('#btn-analyze').disabled = !state.provider;
  $('#btn-analyze').title = state.provider ? '' : 'No signed-in AI tool found';
  $('#btn-scan').textContent = state.listing && state.listing.scanned ? 'Rescan references' : 'Scan references';
}

async function go(path) {
  state.path = path;
  state.selected = null;
  const hash = '#' + encodeURIComponent(path);
  if (location.hash !== hash && !(path === '' && location.hash === '')) history.pushState(null, '', hash);
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
}

// ---- file table ---------------------------------------------------------

function renderFiles() {
  const body = $('#files tbody');
  body.replaceChildren();
  const { entries, analysis, error } = state.listing;
  const items = new Map(((analysis && analysis.items) || []).map((it) => [it.name, it]));
  $('#files').classList.toggle('plain', items.size === 0);
  const empty = $('#files-empty');
  empty.hidden = entries.length > 0;
  empty.textContent = error || 'This folder is empty.';
  for (const e of entries) {
    const it = items.get(e.name);
    const row = h('tr', { class: state.selected === e.name ? 'selected' : '', tabindex: 0 },
      h('td', { class: 'name', title: e.name }, h('span', { class: 'kind', 'aria-hidden': 'true' }, e.dir ? '▸' : ''),
        e.name + (e.dir ? '/' : ''), e.link ? h('span', { class: 'link' }, ' → ' + e.link) : null),
      h('td', { class: 'what ai' }, it ? it.what : ''),
      h('td', { class: 'ai' }, it && it.status && it.status !== 'unknown' ? h('span', { class: 'tag ' + it.status }, it.status) : ''),
      h('td', { class: 'num' }, e.dir ? '' : fmtSize(e.size)),
      h('td', { class: 'when' }, fmtDate(e.mtime)),
      h('td', { class: 'num' }, num(e.refsIn)),
      h('td', { class: 'num' }, num(e.refsOut)));
    const open = () => (e.dir ? go(join(state.path, e.name)) : preview(e.name));
    row.addEventListener('click', open);
    row.addEventListener('keydown', (ev) => { if (ev.key === 'Enter') open(); });
    body.append(row);
  }
}

// ---- right panel --------------------------------------------------------

function showTab(name) {
  state.tab = name;
  for (const b of document.querySelectorAll('.tabs button')) b.setAttribute('aria-selected', b.dataset.tab === name);
  for (const t of ['analysis', 'refs', 'preview']) $('#tab-' + t).hidden = t !== name;
  if (name === 'refs') loadRefs();
}

function impactNode(im) {
  if (!im) return null;
  if (!im.scanned) return h('span', { class: 'impact unknown' }, 'References not scanned yet for these paths');
  if (!im.breaks) return h('span', { class: 'impact safe' }, 'No absolute references point here: a plain move is safe');
  return h('span', { class: 'impact breaks' },
    `A plain move breaks ${im.breaks.toLocaleString()} reference${im.breaks > 1 ? 's' : ''} in ${im.files.toLocaleString()} file${im.files > 1 ? 's' : ''}` +
    (im.authored ? ` (${im.authored.toLocaleString()} in hand-written files)` : ' (all in generated files)'));
}

function renderAnalysis() {
  const box = $('#tab-analysis');
  const a = state.listing.analysis;
  if (!a) {
    fill(box, h('p', { class: 'muted' },
      state.provider
        ? 'No analysis of this folder yet. "Analyze with AI" describes each entry and proposes how to reorganize it. Nothing is moved.'
        : 'No signed-in AI tool was found on this machine, so analysis is unavailable.'));
    return;
  }
  const cats = new Map();
  for (const it of a.items || []) cats.set(it.category || 'other', (cats.get(it.category || 'other') || 0) + 1);
  fill(box, 
    h('h3', {}, 'Summary'),
    h('p', {}, a.summary || ''),
    h('p', { class: 'muted' }, `${a.provider}, ${a.at.slice(0, 16).replace('T', ' ')}`),
    h('h3', {}, 'Topics'),
    h('p', {}, [...cats].sort((x, y) => y[1] - x[1]).map(([c, n]) => h('span', { class: 'tag', style: 'margin:0 4px 4px 0' }, `${c} · ${n}`))),
    h('h3', {}, `Proposals (${(a.proposals || []).length})`),
    (a.proposals || []).length ? null : h('p', { class: 'muted' }, 'Nothing to change here.'),
    (a.proposals || []).map((p) => h('div', { class: 'proposal' },
      h('div', { class: 'op' }, p.op),
      h('div', { class: 'paths' }, p.from.join(', '), h('span', { class: 'muted' }, ' → '), p.to),
      h('div', { class: 'reason' }, p.reason || ''),
      impactNode(p.impact))),
    (a.proposals || []).length ? h('p', { class: 'muted' }, 'Proposals are suggestions only. Applying them is not built yet.') : null);
}

function refNode(r) {
  return h('div', { class: 'ref' },
    r.file + (r.line ? ':' + r.line : ''), h('span', { class: 'arrow' }, ' → '), r.target,
    h('span', { class: 'tag' }, r.category),
    r.cross ? h('span', { class: 'tag' }, 'cross-project') : null,
    r.dangling ? h('span', { class: 'tag dead' }, 'dangling') : null);
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
      'This folder has not been scanned. "Scan references" finds absolute paths in its files and symlinks that point into the workspace: the things a plain move would break.'));
    return;
  }
  const more = (shown, total) => (total > shown ? h('p', { class: 'muted' }, `Showing ${shown} of ${total.toLocaleString()}.`) : null);
  fill(box, 
    h('h3', {}, `Into this folder from elsewhere (${d.inTotal.toLocaleString()})`),
    (d.in || []).length ? (d.in || []).map(refNode) : h('p', { class: 'muted' }, 'None.'),
    more((d.in || []).length, d.inTotal),
    h('h3', {}, `From files in this folder (${d.outTotal.toLocaleString()})`),
    (d.out || []).length ? (d.out || []).map(refNode) : h('p', { class: 'muted' }, 'None.'),
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
      h('p', { class: 'muted' }, `${name} · ${fmtSize(d.size)}` + (d.truncated ? ' · first 64 KB shown' : '')),
      d.binary ? h('p', { class: 'muted' }, 'Binary file, no preview.') : h('pre', { class: 'preview' }, d.text));
  } catch (err) {
    fill(box, h('p', { class: 'muted' }, err.message));
  }
}

// ---- jobs ---------------------------------------------------------------

let pollTimer = null;
const finished = new Set();

async function pollJobs() {
  let jobs;
  try { jobs = (await api('/api/jobs')) || []; } catch { return; }
  const box = $('#jobs');
  fill(box);
  let running = false;
  let changed = false;
  for (const j of jobs.slice(-4).reverse()) {
    const where = j.path || state.root.split('/').pop();
    let text;
    if (j.status === 'running') {
      running = true;
      const secs = Math.round((Date.now() - new Date(j.started)) / 1000);
      text = `${j.kind === 'scan' ? 'Scanning' : 'Analyzing'} ${where}… ${secs}s`;
    } else if (j.status === 'failed') {
      text = `${j.kind} of ${where} failed: ${j.error}`;
    } else {
      text = `${j.kind} of ${where}: ${j.detail}`;
    }
    if (j.status !== 'running' && !finished.has(j.id)) { finished.add(j.id); changed = true; }
    box.append(h('span', { class: 'job ' + j.status }, text));
  }
  if (changed && pollJobs.primed) await reload();
  pollJobs.primed = true;
  clearTimeout(pollTimer);
  if (running) pollTimer = setTimeout(pollJobs, 1500);
}

async function startJob(kind, body) {
  try {
    await api('/api/' + kind, body);
  } catch (err) {
    $('#jobs').replaceChildren(h('span', { class: 'job failed' }, err.message));
    return;
  }
  pollJobs();
}

// ---- start --------------------------------------------------------------

$('#btn-scan').addEventListener('click', () => startJob('scan', { path: state.path }));
$('#btn-analyze').addEventListener('click', () => {
  showTab('analysis');
  startJob('analyze', { path: state.path, provider: state.provider });
});
for (const b of document.querySelectorAll('.tabs button')) b.addEventListener('click', () => showTab(b.dataset.tab));

const fromHash = () => decodeURIComponent(location.hash.slice(1));
window.addEventListener('popstate', () => { if (fromHash() !== state.path) go(fromHash()); });

(async function init() {
  const s = await api('/api/state');
  state.root = s.root;
  state.providers = s.providers || [];
  $('#where').textContent = `${s.host}:${s.root}`;
  document.title = `Arborist · ${s.host}`;
  pickProvider();
  renderProviders();
  renderWelcome();
  await go(fromHash());
  pollJobs();
})();
