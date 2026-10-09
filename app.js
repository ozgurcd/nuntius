'use strict';

// The shell is public; only API requests carry the tab's credential.
let token = new URL(location.href).searchParams.get('token') || '';
try {
  if (token) sessionStorage.setItem('nuntius.web.token', token);
  else token = sessionStorage.getItem('nuntius.web.token') || '';
} catch { /* Storage can be disabled; the start link still works this load. */ }
history.replaceState(null, '', '/' + location.hash);
const el = id => document.getElementById(id);
const cards = new Map(), runs = new Map(), notes = new Map();
const historyTable = el('history').parentElement;
let nextRunID = 0;
let lastData, lastUpdate = 0, refreshing = false, acting = false;
const holds = new WeakMap();
const renderedPanels = new Set();
const tabs = Array.from(document.querySelectorAll('[role="tab"]'));

function selectTab(id, focus = false) {
  const active = tabs.find(tab => tab.getAttribute('aria-controls') === id) || tabs[0];
  let shown = '';
  for (const tab of tabs) {
    const selected = tab === active, panel = el(tab.getAttribute('aria-controls'));
    if (selected && panel.hidden) shown = panel.id;
    tab.setAttribute('aria-selected', String(selected));
    tab.tabIndex = selected ? 0 : -1;
    panel.hidden = !selected;
  }
  history.replaceState(null, '', '/#' + active.getAttribute('aria-controls'));
  if (focus) active.focus();
  if (lastData) renderPanels(lastData, shown);
}
for (const tab of tabs) {
  tab.addEventListener('click', () => selectTab(tab.getAttribute('aria-controls')));
  tab.addEventListener('keydown', event => {
    const index = tabs.indexOf(tab);
    let next;
    if (event.key === 'ArrowLeft') next = (index + tabs.length - 1) % tabs.length;
    if (event.key === 'ArrowRight') next = (index + 1) % tabs.length;
    if (event.key === 'Home') next = 0;
    if (event.key === 'End') next = tabs.length - 1;
    if (next === undefined) return;
    event.preventDefault();
    selectTab(tabs[next].getAttribute('aria-controls'), true);
  });
}
window.addEventListener('hashchange', () => selectTab(location.hash.slice(1)));
selectTab(location.hash.slice(1));

function node(tag, className, text) {
  const n = document.createElement(tag);
  if (className) n.className = className;
  if (text !== undefined) n.textContent = text;
  return n;
}
function setText(n, text) {
  const value = String(text ?? '');
  if (n.textContent !== value) n.textContent = value;
}
function button(text, className, onClick) {
  const b = node('button', className, text);
  b.type = 'button';
  b.addEventListener('click', onClick);
  return b;
}
function hold(event) {
  if (event.type === 'keydown') document.documentElement.classList.add('keyboard-focus');
  if (event.type === 'pointerdown') document.documentElement.classList.remove('keyboard-focus');
  const section = event.target.closest('.agent-card, .panel');
  if (section && event.target !== section) holds.set(section, Date.now() + 5000);
}
document.addEventListener('pointerdown', hold);
document.addEventListener('keydown', hold);
document.addEventListener('wheel', hold, {passive: true});
function busy(n) {
  const selection = window.getSelection();
  return (document.activeElement !== n && n.contains(document.activeElement)) ||
    (holds.get(n) || 0) > Date.now() ||
    (selection && !selection.isCollapsed && n.contains(selection.anchorNode));
}
function duration(seconds) {
  if (seconds == null) return '—';
  let s = Math.max(0, Math.floor(seconds));
  const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60);
  s %= 60;
  return (h ? h + 'h ' : '') + (h || m ? m + 'm ' : '') + s + 's';
}
function relative(value) {
  if (!value || !Number.isFinite(Date.parse(value))) return '—';
  const seconds = Math.max(0, (Date.now() - Date.parse(value)) / 1000);
  if (seconds < 60) return Math.floor(seconds) + 's ago';
  if (seconds < 3600) return Math.floor(seconds / 60) + 'm ago';
  if (seconds < 86400) return Math.floor(seconds / 3600) + 'h ago';
  return Math.floor(seconds / 86400) + 'd ago';
}
function timeText(n, value, prefix = '') {
  setText(n, prefix + relative(value));
  n.title = value ? new Date(value).toLocaleString() : '';
  if (n.tagName === 'TIME' && value) n.dateTime = value;
}
function stateBadge(n, state) {
  const known = ['running', 'idle', 'ended', 'ended_no_report', 'cancelled', 'failed', 'frozen', 'enabled'];
  n.className = 'badge ' + (known.includes(state) ? state : '');
  setText(n, state === 'ended_no_report' ? 'Ended · no report' :
    state === 'idle' ? 'Idle' : state.charAt(0).toUpperCase() + state.slice(1));
}
// Insert/move only where the order actually changed; stable nodes retain focus.
function reconcile(parent, entries, cache, make, update, key) {
  let cursor = parent.tagName === 'TABLE' ? parent.querySelector('thead').nextElementSibling : parent.firstElementChild;
  const wanted = new Set();
  for (const item of entries) {
    const id = key(item);
    wanted.add(id);
    let view = cache.get(id);
    if (!view) { view = make(item); cache.set(id, view); }
    if (view.root !== cursor) parent.insertBefore(view.root, cursor);
    cursor = view.root.nextElementSibling;
    update(view, item);
  }
  for (const [id, view] of cache) {
    if (!wanted.has(id) && !busy(view.root)) { view.root.remove(); cache.delete(id); }
  }
}
function makeCard(s) {
  const root = node('article', 'agent-card');
  root.setAttribute('aria-label', s.lane);
  const head = node('div', 'card-heading'), name = node('h3', '', s.lane);
  const kind = node('p', 'kind'), badge = node('span', 'badge');
  const identity = node('div'); identity.append(name, kind); head.append(identity, badge);
  const goal = node('p', 'goal'), stats = node('dl', 'card-stats'), fields = {};
  for (const label of ['Elapsed', 'ETA', 'Last event', 'Report']) {
    const group = node('div'), value = node('dd');
    group.append(node('dt', '', label), value); stats.append(group); fields[label] = value;
  }
  const activity = node('p', 'activity'), activityText = node('span');
  activity.append(node('span', 'activity-label', 'Current activity'), activityText);
  const tail = node('details'), tailText = node('pre', 'log');
  tail.append(node('summary', '', 'View tail'), tailText);
  const controls = node('div', 'card-actions'), cancelControls = node('span');
  controls.append(button('Message', '', () => {
    selectTab('messages');
    el('lane').value = s.lane;
    el('message').focus();
  }));
  const confirm = node('div', 'confirm'); confirm.hidden = true;
  let confirmTimer;
  function resetCancel() {
    clearTimeout(confirmTimer);
    const restoreFocus = confirm.contains(document.activeElement);
    confirm.hidden = true; cancel.hidden = false;
    if (restoreFocus) cancel.focus();
  }
  const cancel = button('Cancel run', 'danger', () => {
    cancel.hidden = true; confirm.hidden = false; yes.focus();
    confirmTimer = setTimeout(resetCancel, 15000);
  });
  const yes = button('Confirm cancel', 'danger', async () => {
    if (await action('cancel', s.lane)) resetCancel();
  });
  const back = button('Keep running', '', resetCancel);
  confirm.append(node('p', '', 'Interrupt this run?'), yes, back);
  cancelControls.append(cancel); controls.append(cancelControls);
  root.append(head, goal, stats, activity, tail, controls, confirm);
  return {root, kind, badge, goal, fields, activityText, tail, tailText, cancelControls, confirm, resetCancel};
}
function updateCard(v, s) {
  if (busy(v.root)) return;
  setText(v.kind, s.kind || 'Agent');
  stateBadge(v.badge, s.state);
  v.goal.className = s.goal_id ? 'goal' : 'goal goal-placeholder';
  setText(v.goal, s.goal_id || 'Ready for the next goal');
  setText(v.fields.Elapsed, s.goal_id ? duration(s.elapsed_seconds) : '—');
  v.fields.Elapsed.title = s.started ? 'Started ' + new Date(s.started).toLocaleString() : '';
  setText(v.fields.ETA, s.eta === '-' ? 'Not enough history' : s.eta || '—');
  setText(v.fields['Last event'], s.last_event_age_seconds == null ? '—' : duration(s.last_event_age_seconds) + ' ago');
  v.fields['Last event'].title = s.last_event ? new Date(s.last_event).toLocaleString() : '';
  setText(v.fields.Report, s.report_exists ? 'Yes' : 'No');
  setText(v.activityText, s.activity === '-' ? 'No recent activity' : s.activity || 'No recent activity');
  v.activityText.title = v.activityText.textContent;
  // An open tail is a reading surface: resume text updates when it is closed.
  if (!v.tail.open) setText(v.tailText, (lastData.tails[s.lane] || []).join('\n') || 'No output yet.');
  v.cancelControls.hidden = s.state !== 'running';
  if (s.state !== 'running') v.resetCancel();
}
function makeRun(r) {
  // Each tbody is a keyed group of its compact row and expanded detail row.
  const root = node('tbody'), row = node('tr', 'run-row'), detail = node('tr', 'run-detail');
  const lane = node('td', 'run-lane'), goal = node('td', 'run-goal'), state = node('td', 'run-state'), started = node('td', 'run-started'), elapsed = node('td', 'run-duration');
  const toggle = button('', 'run-toggle', () => {
    detail.hidden = !detail.hidden;
    toggle.setAttribute('aria-expanded', String(!detail.hidden));
    setText(toggle, (detail.hidden ? '▸ ' : '▾ ') + view.data.goal_id);
    if (!detail.hidden) updateRunContent(view, view.data);
  });
  toggle.setAttribute('aria-expanded', 'false');
  const content = node('td'); content.colSpan = 5;
  content.id = 'run-' + nextRunID++;
  toggle.setAttribute('aria-controls', content.id);
  const badge = node('span', 'badge'), timestamp = node('time');
  goal.append(toggle); state.append(badge); started.append(timestamp);
  row.append(lane, goal, state, started, elapsed);
  const sections = {};
  for (const name of ['QUESTIONS', 'DECISIONS']) {
    const box = node('div', name.toLowerCase()), pre = node('pre', 'report-text');
    box.append(node('h3', '', name.charAt(0) + name.slice(1).toLowerCase()), pre);
    content.append(box); sections[name] = pre;
  }
  const goalText = node('pre', 'report-text'), report = node('pre', 'report-text');
  for (const [name, pre] of [['Goal', goalText], ['Report', report]]) {
    const block = node('div', 'report-block'); block.append(node('h3', '', name), pre); content.append(block);
  }
  detail.append(content); detail.hidden = true; root.append(row, detail);
  const view = {root, lane, toggle, badge, timestamp, elapsed, detail, sections, goalText, report, data: r};
  return view;
}
function updateRunContent(v, r) {
  for (const name of ['QUESTIONS', 'DECISIONS']) setText(v.sections[name], r.sections[name] || 'None recorded.');
  setText(v.goalText, r.goal || 'No goal text available.');
  setText(v.report, r.report || 'No report available.');
}
function updateRun(v, r) {
  v.data = r;
  setText(v.lane, r.lane);
  setText(v.toggle, (v.detail.hidden ? '▸ ' : '▾ ') + r.goal_id);
  stateBadge(v.badge, r.state === 'ended' && r.exit != null && r.exit !== 0 ? 'failed' : r.state);
  timeText(v.timestamp, r.started);
  const end = r.ended ? Date.parse(r.ended) : Date.now();
  setText(v.elapsed, duration((end - Date.parse(r.started)) / 1000));
  // Expanded reports never get rewritten underneath the reader.
  if (v.detail.hidden && !busy(el('runs'))) updateRunContent(v, r);
}
function filterRuns() {
  const query = el('filter').value.toLowerCase().trim();
  let visible = 0;
  for (const v of runs.values()) {
    const match = (v.data.lane + ' ' + v.data.goal_id).toLowerCase().includes(query);
    v.root.hidden = !match;
    if (match) visible++;
  }
  el('historyEmpty').hidden = visible !== 0;
}
function makeNote(n) {
  const root = node('article', 'note'), header = node('div', 'note-header');
  const timestamp = node('time'), read = node('span'), text = node('p');
  header.append(timestamp, read); root.append(header, text);
  return {root, timestamp, read, text};
}
function render(d) {
  lastData = d;
  const frozen = !!d.freeze;
  stateBadge(el('freeze'), frozen ? 'frozen' : 'enabled');
  if (frozen) {
    timeText(el('freeze'), d.freeze.since, 'Frozen since ');
    setText(el('freeze'), el('freeze').textContent + ' — ' + d.freeze.reason);
  } else setText(el('freeze'), 'Dispatch enabled');
  el('freezeButton').hidden = frozen; el('unfreezeButton').hidden = !frozen;
  setText(el('runningCount'), d.lanes.filter(s => s.state === 'running').length);
  setText(el('idleCount'), d.lanes.filter(s => s.state === 'idle').length);
  setText(el('endedCount'), d.lanes.filter(s => !['running', 'idle'].includes(s.state)).length);
  setText(el('agentsTabCount'), d.lanes.filter(s => s.state === 'running').length);
  setText(el('notesTabCount'), d.notes.filter(n => !n.read).length);
  setText(el('questionsTabCount'), d.history.filter(r => {
    const questions = (r.sections.QUESTIONS || '').trim();
    return questions && !/^(?:none|none recorded|no open questions)[.!]?$/i.test(questions);
  }).length);
  renderPanels(d);
}
function panelReady(id, shown) {
  const panel = el(id);
  return !panel.hidden && (id === shown || !renderedPanels.has(id) || !busy(panel));
}
function renderPanels(d, shown = '') {
  if (!el('agents').hidden) {
    reconcile(el('lanes'), d.lanes, cards, makeCard, updateCard, s => s.lane);
    el('agentsEmpty').hidden = d.lanes.length !== 0;
  }
  if (panelReady('messages', shown)) {
    const select = el('lane'), chosen = select.value;
    for (const option of Array.from(select.options)) if (!d.lanes.some(s => s.lane === option.value)) option.remove();
    for (const s of d.lanes) if (!Array.from(select.options).some(o => o.value === s.lane)) {
      const option = node('option', '', s.lane); option.value = s.lane; select.append(option);
    }
    if (d.lanes.some(s => s.lane === chosen)) select.value = chosen;
    reconcile(el('notes'), d.notes, notes, makeNote, (v, n) => {
      timeText(v.timestamp, n.time); setText(v.read, n.read ? 'Read' : 'Unread'); setText(v.text, n.text);
    }, n => n.id);
    el('notesEmpty').hidden = d.notes.length !== 0;
    renderedPanels.add('messages');
  }
  const digest = el('oversight');
  if (panelReady('digest', shown) && digest.dataset.value !== d.oversight) {
    digest.dataset.value = d.oversight;
    const lines = (d.oversight || 'No owner digest yet.').split('\n');
    digest.replaceChildren(...lines.map(line => {
      const heading = line.match(/^(#{1,6}) (.*)$/);
      return node(heading ? 'h' + Math.min(4, heading[1].length + 2) : 'p', '', heading ? heading[2] : line);
    }));
    renderedPanels.add('digest');
  }
  // History groups are sibling tbodies, following the fixed table head.
  if (panelReady('runs', shown)) {
    reconcile(historyTable, d.history, runs, makeRun, updateRun, r => r.run_id);
    filterRuns();
    renderedPanels.add('runs');
  }
  if (panelReady('auditSection', shown) && (!renderedPanels.has('auditSection') || !el('auditSection').querySelector('details').open)) {
    setText(el('audit'), d.audit || 'No audit events yet.');
    renderedPanels.add('auditSection');
  }
}
// The placeholder tbody is removed once; the table head is never reconciled.
el('history').remove();
async function refresh() {
  if (!token) {
    setText(el('error'), 'Open the link printed by `nuntius web` to sign in.');
    el('error').hidden = false;
    return;
  }
  if (refreshing || acting || document.hidden) return;
  refreshing = true;
  try {
    const response = await fetch('/data', {headers: {'X-Nuntius-Token': token}, cache: 'no-store'});
    if (!response.ok) throw new Error('unavailable');
    const d = await response.json();
    render(d);
    lastUpdate = Date.now();
    setText(el('lastRefresh'), 'Last refresh ' + new Date(lastUpdate).toLocaleTimeString());
    el('lastRefresh').dateTime = new Date(lastUpdate).toISOString();
    el('lastRefresh').title = new Date(lastUpdate).toLocaleString();
    el('updated').title = el('lastRefresh').title;
    el('error').hidden = true;
  } catch {
    setText(el('error'), 'Unable to refresh dashboard. Showing the last received data. Retrying every 2 seconds.');
    el('error').hidden = false;
  } finally { refreshing = false; }
}
async function action(name, lane = '', text = '', stop = false) {
  if (!token) { await refresh(); return false; }
  if (acting) return false;
  acting = true;
  const buttons = Array.from(document.querySelectorAll('button'));
  buttons.forEach(b => { b.disabled = true; });
  let ok = false;
  try {
    const response = await fetch('/action', {method: 'POST', headers: {'X-Nuntius-Token': token, 'Content-Type': 'application/json'}, body: JSON.stringify({action: name, lane, text, stop})});
    if (!response.ok) throw new Error('refused');
    setText(el('notice'), name === 'message' ? 'Message sent or queued.' : name === 'note' ? 'Note saved.' : name === 'cancel' ? 'Cancellation requested.' : name === 'freeze' ? 'Dispatch frozen.' : 'Dispatch enabled.');
    el('notice').hidden = false; el('error').hidden = true; ok = true;
  } catch {
    setText(el('error'), 'Action could not be completed. Check the current state and try again.');
    el('error').hidden = false;
  } finally {
    acting = false; buttons.forEach(b => { b.disabled = false; });
  }
  if (ok) await refresh();
  return ok;
}
el('freezeButton').addEventListener('click', () => action('freeze', '', 'Owner dashboard freeze'));
el('unfreezeButton').addEventListener('click', () => action('unfreeze'));
el('messageForm').addEventListener('submit', async event => {
  event.preventDefault();
  if (await action('message', el('lane').value, el('message').value)) el('message').value = '';
});
el('stopSend').addEventListener('click', async () => {
  if (await action('message', el('lane').value, el('message').value, true)) el('message').value = '';
});
el('noteForm').addEventListener('submit', async event => {
  event.preventDefault();
  if (await action('note', '', el('note').value)) el('note').value = '';
});
el('filter').addEventListener('input', filterRuns);
document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(); });
setInterval(() => {
  if (lastUpdate) setText(el('updated'), 'Updated ' + Math.floor((Date.now() - lastUpdate) / 1000) + 's ago');
}, 1000);
refresh();
setInterval(refresh, 2000);
