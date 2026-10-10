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
const agentTabs = new Map(), laneRuns = new Map(), audits = new Map();
let laneSelection = '';

function selectTab(id, focus = false, push = true) {
  const parts = id.split('/');
  laneSelection = parts[0] === 'agents' ? parts[1] || '' : laneSelection;
  id = parts[0];
  const active = tabs.find(tab => tab.getAttribute('aria-controls') === id) || tabs[0];
  let shown = '';
  for (const tab of tabs) {
    const selected = tab === active, panel = el(tab.getAttribute('aria-controls'));
    if (selected && panel.hidden) shown = panel.id;
    tab.setAttribute('aria-selected', String(selected));
    tab.tabIndex = selected ? 0 : -1;
    panel.hidden = !selected;
  }
  const panelID = active.getAttribute('aria-controls');
  const hash = '#' + panelID + (panelID === 'agents' && laneSelection ? '/' + laneSelection : '');
  if (location.hash !== hash) history[push ? 'pushState' : 'replaceState'](null, '', '/' + hash);
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
window.addEventListener('hashchange', () => selectTab(location.hash.slice(1), false, false));
selectTab(location.hash.slice(1), false, false);

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
// The server names the colour (badge: green, amber, red or none); the label is
// never parsed, so wording changes cannot change a colour.
function stateBadge(n, state, tone = '') {
  n.className = tone ? 'badge ' + tone : 'badge';
  setText(n, state === 'ended_no_report' ? 'Ended · no report' :
    state === 'timed_out' ? 'Timed out' : state.charAt(0).toUpperCase() + state.slice(1));
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
  for (const label of ['Run', 'Elapsed', 'ETA', 'Last event', 'Report']) {
    const group = node('div'), value = node('dd');
    group.append(node('dt', '', label), value); stats.append(group); fields[label] = value;
  }
  const activity = node('p', 'activity'), activityText = node('span');
  activity.append(node('span', 'activity-label', 'Current action'), activityText);
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
  setText(v.kind, (s.kind || 'Agent') + ' · model=' + (s.model || 'default') + ' effort=' + (s.effort || 'default'));
  stateBadge(v.badge, s.display_state || s.state, s.badge);
  v.goal.className = s.goal_id ? 'goal' : 'goal goal-placeholder';
  setText(v.goal, s.goal_id || 'Ready for the next goal');
  setText(v.fields.Elapsed, s.goal_id ? duration(s.elapsed_seconds) : '—');
  setText(v.fields.Run, s.run_id || '—');
  v.fields.Elapsed.title = s.started ? 'Started ' + new Date(s.started).toLocaleString() : '';
  setText(v.fields.ETA, s.eta === '-' ? 'Not enough history' : s.eta || '—');
  setText(v.fields['Last event'], s.last_event_age_seconds == null ? '—' : duration(s.last_event_age_seconds) + ' ago');
  v.fields['Last event'].title = s.last_event ? new Date(s.last_event).toLocaleString() : '';
  setText(v.fields.Report, s.report_exists ? 'Yes' : 'No');
  const a = s.current_action;
  const estimate = a && a.age_source !== 'event' ? '~' : '';
  setText(v.activityText, a ? a.summary + ' (' + estimate + a.age_seconds + 's ago)' +
    (a.running_seconds == null ? '' : '; running for ' + estimate + a.running_seconds + 's') :
    s.activity === '-' ? 'No recent activity' : s.activity || 'No recent activity');
  v.activityText.title = v.activityText.textContent;
  // An open tail is a reading surface: resume text updates when it is closed.
  if (!v.tail.open) setText(v.tailText, [...(lastData.tails[s.lane] || [])].reverse().join('\n') || 'No output yet.');
  v.cancelControls.hidden = s.state !== 'running';
  if (s.state !== 'running') v.resetCancel();
}
function makeRun(r) {
  // Each tbody is a keyed group of its compact row and expanded detail row.
  const root = node('tbody'), row = node('tr', 'run-row'), detail = node('tr', 'run-detail');
  const lane = node('td', 'run-lane'), runID = node('td', 'run-id'), goal = node('td', 'run-goal'), state = node('td', 'run-state'), started = node('td', 'run-started'), elapsed = node('td', 'run-duration'), relation = node('td', 'run-relation');
  const toggle = button('', 'run-toggle', () => {
    detail.hidden = !detail.hidden;
    toggle.setAttribute('aria-expanded', String(!detail.hidden));
    setText(toggle, (detail.hidden ? '▸ ' : '▾ ') + view.data.goal_id);
    if (!detail.hidden) updateRunContent(view, view.data);
  });
  toggle.setAttribute('aria-expanded', 'false');
  const identity = node('td', 'run-identity');
  const content = node('td'); content.colSpan = 8;
  content.id = 'run-' + nextRunID++;
  toggle.setAttribute('aria-controls', content.id);
  const badge = node('span', 'badge'), timestamp = node('time');
  const reportLink = node('a', 'report-link', 'Report');
  reportLink.href = '#runs';
  reportLink.addEventListener('click', event => {
    event.preventDefault(); selectTab('runs');
    const target = runs.get(view.data.run_id);
    if (target) { target.detail.hidden = false; target.toggle.setAttribute('aria-expanded', 'true'); updateRunContent(target, target.data); target.toggle.focus(); }
  });
  goal.append(toggle, reportLink); state.append(badge); started.append(timestamp);
  row.append(lane, runID, goal, state, started, elapsed, relation, identity);
  const sections = {};
  for (const name of ['QUESTIONS', 'DECISIONS']) {
    const box = node('div', name.toLowerCase()), pre = node('pre', 'report-text');
    box.append(node('h3', '', name.charAt(0) + name.slice(1).toLowerCase()), pre);
    content.append(box); sections[name] = pre;
  }
  const chain = node('p', 'muted'); content.append(chain);
  const goalText = node('pre', 'report-text'), report = node('pre', 'report-text');
  for (const [name, pre] of [['Goal', goalText], ['Report', report]]) {
    const block = node('div', 'report-block'); block.append(node('h3', '', name), pre); content.append(block);
  }
  detail.append(content); detail.hidden = true; root.append(row, detail);
  const view = {root, lane, runID, relation, identity, reportLink, toggle, badge, timestamp, elapsed, detail, sections, chain, goalText, report, data: r};
  return view;
}
function updateRunContent(v, r) {
  setText(v.chain, [r.relation || 'first run', r.kind, 'model=' + (r.model || 'default') + ' effort=' + (r.effort || 'default')].filter(Boolean).join(' · '));
  for (const name of ['QUESTIONS', 'DECISIONS']) setText(v.sections[name], r.sections[name] || 'None recorded.');
  setText(v.goalText, r.goal || 'No goal text available.');
  setText(v.report, r.report || 'No report available.');
}
function updateRun(v, r) {
  v.data = r;
  v.reportLink.hidden = !r.report;
  setText(v.lane, r.lane);
  setText(v.runID, r.run_id);
  setText(v.relation, r.relation || 'first run');
  setText(v.identity, 'model=' + (r.model || 'default') + ' effort=' + (r.effort || 'default'));
  v.toggle.className = r.chain_depth > 0 ? 'run-toggle chain-child' : 'run-toggle';
  setText(v.toggle, (v.detail.hidden ? '▸ ' : '▾ ') + r.goal_id);
  const state = r.display_state || r.state;
  stateBadge(v.badge, state === 'ended' && r.exit != null && r.exit !== 0 ? 'failed' : state, r.badge);
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
function fillFilter(select, values, label) {
  const chosen = select.value;
  const wanted = ['', ...new Set(values.filter(Boolean))];
  for (const option of Array.from(select.options)) if (!wanted.includes(option.value)) option.remove();
  for (const value of wanted) if (!Array.from(select.options).some(o => o.value === value)) {
    const option = node('option', '', value || label); option.value = value; select.append(option);
  }
  select.value = wanted.includes(chosen) ? chosen : '';
}
function laneIndicator(s) {
  const state = (s.display_state || s.state || 'idle').toLowerCase();
  if (s.exit > 0 || s.badge === 'red' || /^(failed|interrupted|cancelled|timed_out|timed out|ended_no_report)/.test(state)) return {kind:'failed', label:'Failed or interrupted'};
  if (state === 'stuck?' || (s.state === 'running' && (s.last_event_age_seconds > 900 || (!s.last_event && s.elapsed_seconds > 900)))) return {kind:'stuck', label:'Stuck: no recent progress'};
  if (/^(ended|completed)/.test(state)) return {kind:'finished', label:'Finished'};
  if (/^running/.test(state)) return {kind:'running', label:'Running'};
  return {kind:'idle', label:'Idle'};
}
function renderAgentTabs(d) {
  if (laneSelection && !d.lanes.some(s => s.lane === laneSelection)) {
    laneSelection = '';
    if (!el('agents').hidden) history.replaceState(null, '', '/#agents');
  }
  reconcile(el('agentTabs'), [{lane: '', state:'all'}, ...d.lanes], agentTabs, s => {
    const root = button(s.lane || 'All', '', () => selectTab('agents' + (s.lane ? '/' + s.lane : '')));
    root.setAttribute('role', 'tab'); root.setAttribute('aria-controls', 'lanes');
    root.addEventListener('keydown', event => {
      const names = ['', ...lastData.lanes.map(v => v.lane)], index = names.indexOf(s.lane);
      let next;
      if (event.key === 'ArrowLeft') next = (index + names.length - 1) % names.length;
      if (event.key === 'ArrowRight') next = (index + 1) % names.length;
      if (event.key === 'Home') next = 0;
      if (event.key === 'End') next = names.length - 1;
      if (next === undefined) return;
      event.preventDefault(); selectTab('agents' + (names[next] ? '/' + names[next] : ''));
      agentTabs.get(names[next]).root.focus();
    });
    const indicator = node('span', 'lane-indicator');
    indicator.setAttribute('aria-hidden', 'true');
    if (s.lane) root.append(indicator);
    return {root, indicator};
  }, (v,s) => {
    const selected = s.lane === laneSelection;
    v.root.setAttribute('aria-selected', String(selected)); v.root.tabIndex = selected ? 0 : -1;
    v.root.className = 'agent-tab';
    const mark = laneIndicator(s);
    v.indicator.className = 'lane-indicator indicator-' + mark.kind;
    v.root.title = s.lane ? mark.label + ' · ' + (s.display_state || s.state) : 'All agents';
    v.root.setAttribute('aria-label', s.lane ? s.lane + ': ' + mark.label : 'All agents');
  }, s => s.lane);
}
function renderLane(d) {
  el('laneDetail').hidden = !laneSelection;
  if (!laneSelection) return;
  if (el('followTail').checked && !busy(el('laneDetail'))) setText(el('laneTail'), [...(d.tails[laneSelection] || [])].reverse().join('\n') || 'No output yet.');
  const laneHistory = d.history.filter(r => r.lane === laneSelection);
  reconcile(el('laneHistory'), laneHistory, laneRuns, makeRun, updateRun, r => r.run_id);
  el('laneHistoryEmpty').hidden = laneHistory.length !== 0;
  const own = d.notes.filter(n => n.lane === laneSelection);
  setText(el('laneNotes'), own.map(n => n.text).join('\n\n') || 'No owner notes for this agent.');
  setText(el('laneSignals'), ((d.signals || {})[laneSelection] || []).join('\n') || 'No pending signals.');
}
function makeAudit(e) {
  const root = node('tr'), timestamp = node('time'), summary = node('td'), lane = node('td'), goal = node('td'), result = node('td'), digest = node('td');
  const when = node('td'); when.append(timestamp);
  const raw = node('details'), rawText = node('pre', 'log'); raw.append(node('summary', '', 'Raw'), rawText);
  summary.append(node('span'), raw);
  root.append(when, summary, lane, goal, result, digest);
  return {root, timestamp, summary: summary.firstElementChild, lane, goal, result, digest, rawText};
}
function updateAudit(v,e) {
  v.data = e; timeText(v.timestamp,e.time);
  setText(v.summary,e.summary); setText(v.lane,e.lane || '—'); setText(v.goal,e.goal_id || '—');
  setText(v.result,e.result + (e.reason ? ': ' + e.reason : '')); setText(v.digest,e.digest || '—'); setText(v.rawText,e.raw);
}
function filterAudit() {
  const query = el('auditFilter').value.toLowerCase().trim();
  let visible = 0;
  for (const v of audits.values()) {
    const e = v.data;
    const match = (!el('auditLane').value || e.lane === el('auditLane').value) &&
      (!el('auditAction').value || e.action === el('auditAction').value) &&
      [e.summary,e.lane,e.goal_id,e.result,e.reason,e.digest].join(' ').toLowerCase().includes(query);
    v.root.hidden = !match; if (match) visible++;
  }
  el('auditEmpty').hidden = visible !== 0;
}
function render(d) {
  lastData = d;
  const frozen = !!d.freeze;
  stateBadge(el('freeze'), frozen ? 'frozen' : 'enabled', frozen ? 'red' : 'green');
  if (frozen) {
    timeText(el('freeze'), d.freeze.since, 'Frozen since ');
    setText(el('freeze'), el('freeze').textContent + ' — ' + d.freeze.reason);
  } else setText(el('freeze'), 'Dispatch enabled');
  const fullFreezeText = el('freeze').textContent;
  el('freeze').title = fullFreezeText;
  el('freeze').setAttribute('aria-label', fullFreezeText);
  const full = node('span', 'freeze-full', fullFreezeText);
  const short = node('span', 'freeze-short', frozen ? 'Frozen' : 'Enabled');
  full.setAttribute('aria-hidden', 'true'); short.setAttribute('aria-hidden', 'true');
  el('freeze').replaceChildren(full, short);
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
    renderAgentTabs(d);
    reconcile(el('lanes'), d.lanes, cards, makeCard, updateCard, s => s.lane);
    for (const [lane,v] of cards) v.root.hidden = !d.lanes.some(s => s.lane === lane) || (!!laneSelection && lane !== laneSelection);
    renderLane(d);
    el('agentsEmpty').hidden = d.lanes.length !== 0;
  }
  if (panelReady('messages', shown)) {
    fillFilter(el('noteLane'), d.lanes.map(s => s.lane), 'Judge (shared)');
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
  if (panelReady('auditSection', shown)) {
    fillFilter(el('auditLane'), d.audit.map(e => e.lane), 'All lanes');
    fillFilter(el('auditAction'), d.audit.map(e => e.action), 'All actions');
    reconcile(el('auditRows'), d.audit.map((e,i) => ({...e, rowKey: e.time + ':' + e.raw + ':' + i})), audits, makeAudit, updateAudit, e => e.rowKey);
    filterAudit();
    renderedPanels.add('auditSection');
  }
}
// The placeholder tbody is removed once; the table head is never reconciled.
el('history').remove();
el('laneHistoryPlaceholder').remove();
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
  if (await action('note', el('noteLane').value, el('note').value)) el('note').value = '';
});
el('filter').addEventListener('input', filterRuns);
el('auditFilter').addEventListener('input', filterAudit);
el('auditLane').addEventListener('change', filterAudit);
el('auditAction').addEventListener('change', filterAudit);
el('followTail').addEventListener('change', () => { if (lastData) renderLane(lastData); });
document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(); });
setInterval(() => {
  if (lastUpdate) setText(el('updated'), 'Updated ' + Math.floor((Date.now() - lastUpdate) / 1000) + 's ago');
}, 1000);
refresh();
setInterval(refresh, 2000);
