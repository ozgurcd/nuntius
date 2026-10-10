'use strict';

// The shell is public; only API headers carry the credential. Remember it for
// this browser origin, with an absolute expiry that reloads never extend.
const credentialKey = 'nuntius.web.credential';
const credentialLifetime = 7 * 24 * 60 * 60 * 1000;
let token = new URL(location.href).searchParams.get('token') || '';
history.replaceState(null, '', '/' + location.hash);
let expiresAt = token ? Date.now() + credentialLifetime : 0;
let authEpoch = 0;
try {
  sessionStorage.removeItem('nuntius.web.token');
} catch { /* Legacy tab storage may be disabled independently. */ }
try {
  if (token) localStorage.setItem(credentialKey, JSON.stringify({token, expiresAt}));
  else {
    const saved = JSON.parse(localStorage.getItem(credentialKey) || 'null');
    if (saved && typeof saved.token === 'string' && saved.token &&
        Number.isSafeInteger(saved.expiresAt) && saved.expiresAt > Date.now() &&
        saved.expiresAt <= Date.now() + credentialLifetime) {
      token = saved.token; expiresAt = saved.expiresAt;
    } else localStorage.removeItem(credentialKey);
  }
} catch { /* Storage can be disabled; the start link still works this load. */ }
// Malformed JSON must not remain a remembered sign-in.
if (!token) {
  try { localStorage.removeItem(credentialKey); } catch {}
}
function signOut(removeStored = true) {
  if (!token) return;
  token = ''; expiresAt = 0; authEpoch++;
  clearSettings();
  if (removeStored) {
    try { localStorage.removeItem(credentialKey); } catch {}
  }
  try { sessionStorage.removeItem('nuntius.web.token'); } catch {}
  // A fresh public shell clears private data, drafts and any pending requests.
  location.reload();
}
function authenticated() {
  if (token && Date.now() >= expiresAt) signOut();
  return !!token;
}
window.addEventListener('storage', event => {
  if (event.storageArea === localStorage && (event.key === credentialKey || event.key === null)) {
    // Another tab may have signed in with a new grant. Preserve its new record.
    signOut(false);
  }
});
const el = id => document.getElementById(id);
el('signOut').hidden = !token;
el('signOut').addEventListener('click', () => signOut());
const cards = new Map(), runs = new Map(), notes = new Map();
const historyTable = el('history').parentElement;
let nextRunID = 0;
let lastData, lastUpdate = 0, refreshing = false, acting = false;
const holds = new WeakMap();
const renderedPanels = new Set();
const tabs = Array.from(document.querySelectorAll('[role="tab"]'));
const agentTabs = new Map(), laneRuns = new Map(), audits = new Map();
let laneSelection = '';
const settingsKinds = ['claude', 'codex', 'opencode'];
let settingsCurrent = null, settingsDraft = null, settingsLoading = false, settingsSaving = false;
let settingsReadAt = 0, settingsConfirmed = false;
let settingsErrors = {};
let settingsReadonlyID = 0;
const auditReadsKey = 'nuntius.web.audit.showReads';
try {
  el('auditReads').checked = localStorage.getItem(auditReadsKey) === 'true';
} catch { /* Filtering still works when browser storage is disabled. */ }

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
  if (panelID === 'settings') {
    active.scrollIntoView?.({block:'nearest', inline:'nearest'});
    loadSettings();
  }
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
	const explanation = s.eta_details ? s.eta_details.explanation : '';
	v.fields.ETA.title = explanation;
	v.fields.ETA.tabIndex = explanation ? 0 : -1;
	v.fields.ETA.onclick = () => { if (explanation) alert(explanation); };
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
  if (s.state !== 'running' && s.outcome === 'report_delivery_failed') return {kind:'attention', label:'Finished with attention: report missing'};
  if (s.exit > 0 || s.badge === 'red' || s.state === 'ended_no_report' || /^(failed|interrupted|cancelled|timed_out|timed out|ended_no_report)/.test(state)) return {kind:'failed', label:'Failed, interrupted or missing report'};
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
  setText(v.summary,e.summary); setText(v.lane,e.lane || 'all lanes'); setText(v.goal,e.goal_id || 'n/a');
  v.goal.className = e.goal_id ? '' : 'muted';
  setText(v.result,e.result + (e.reason ? ': ' + e.reason : '')); setText(v.digest,e.digest || 'n/a'); setText(v.rawText,e.raw);
  v.digest.className = e.digest ? '' : 'muted';
}
function filterAudit() {
  const query = el('auditFilter').value.toLowerCase().trim();
  let visible = 0;
  for (const v of audits.values()) {
    const e = v.data;
    const match = (el('auditReads').checked || e.read_only !== true) &&
      (!el('auditLane').value || e.lane === el('auditLane').value) &&
      (!el('auditAction').value || e.action === el('auditAction').value) &&
      [e.summary,e.lane,e.goal_id,e.result,e.reason,e.digest].join(' ').toLowerCase().includes(query);
    v.root.hidden = !match; if (match) visible++;
  }
  el('auditEmpty').hidden = visible !== 0;
  setText(el('auditCount'), visible + ' of ' + audits.size + ' entries');
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
  if (!authenticated()) {
    setText(el('error'), 'Open the link printed by `nuntius web` to sign in.');
    el('error').hidden = false;
    return;
  }
  if (refreshing || acting || document.hidden) return;
  refreshing = true;
  const epoch = authEpoch;
  try {
    const response = await fetch('/data', {headers: {'X-Nuntius-Token': token}, cache: 'no-store'});
    if (epoch !== authEpoch || !authenticated()) return;
    if (response.status === 403) { signOut(); return; }
    if (!response.ok) throw new Error('unavailable');
    const d = await response.json();
    if (epoch !== authEpoch || !authenticated()) return;
    render(d);
    lastUpdate = Date.now();
    setText(el('lastRefresh'), 'Last refresh ' + new Date(lastUpdate).toLocaleTimeString());
    el('lastRefresh').dateTime = new Date(lastUpdate).toISOString();
    el('lastRefresh').title = new Date(lastUpdate).toLocaleString();
    el('updated').title = el('lastRefresh').title;
    el('error').hidden = true;
  } catch {
    if (epoch !== authEpoch || !authenticated()) return;
    setText(el('error'), 'Unable to refresh dashboard. Showing the last received data. Retrying every 2 seconds.');
    el('error').hidden = false;
  } finally { refreshing = false; }
}
async function action(name, lane = '', text = '', stop = false) {
  if (!authenticated()) { await refresh(); return false; }
  if (acting) return false;
  acting = true;
  // Local sign-out must remain available even if an action never settles.
  const buttons = Array.from(document.querySelectorAll('button')).filter(b => b !== el('signOut'));
  buttons.forEach(b => { b.disabled = true; });
  let ok = false;
  const epoch = authEpoch;
  try {
    const response = await fetch('/action', {method: 'POST', headers: {'X-Nuntius-Token': token, 'Content-Type': 'application/json'}, body: JSON.stringify({action: name, lane, text, stop})});
    if (epoch !== authEpoch || !authenticated()) return false;
    if (response.status === 403) { signOut(); return false; }
    if (!response.ok) throw new Error('refused');
    setText(el('notice'), name === 'message' ? 'Message sent or queued.' : name === 'note' ? 'Note saved.' : name === 'cancel' ? 'Cancellation requested.' : name === 'freeze' ? 'Dispatch frozen.' : 'Dispatch enabled.');
    el('notice').hidden = false; el('error').hidden = true; ok = true;
  } catch {
    if (epoch !== authEpoch || !authenticated()) return false;
    setText(el('error'), 'Action could not be completed. Check the current state and try again.');
    el('error').hidden = false;
  } finally {
    acting = false; buttons.forEach(b => { b.disabled = false; }); settingsUpdate();
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
el('auditReads').addEventListener('change', () => {
  try { localStorage.setItem(auditReadsKey, String(el('auditReads').checked)); } catch {}
  filterAudit();
});
el('followTail').addEventListener('change', () => { if (lastData) renderLane(lastData); });
document.addEventListener('visibilitychange', () => { if (!document.hidden) { refresh(); if (!el('settings').hidden) loadSettings(); } });
window.addEventListener('focus', () => { if (!el('settings').hidden) loadSettings(); });
window.addEventListener('beforeunload', event => { if (token && settingsDiff().length) { event.preventDefault(); event.returnValue = ''; } });

function settingsDiff() {
  if (!settingsCurrent || !settingsDraft) return [];
  const changes = settingsKinds.filter(k => settingsDraft[k] !== String(settingsCurrent.pools[k].size)).map(k => [k, settingsDraft[k]]);
  if (settingsDraft.notify !== settingsCurrent.notify_imessage_enabled) changes.push(['notify', settingsDraft.notify]);
  return changes;
}
function settingsBanner(text, error = false) {
  setText(el('settingsBanner'), text); el('settingsBanner').hidden = !text;
  el('settingsBanner').className = error ? 'error' : 'notice';
  el('settingsBanner').setAttribute('role', error ? 'alert' : 'status');
}
function settingsFieldError(k, text) {
  setText(el('error-' + k), text); el('error-' + k).hidden = !text;
  el('setting-' + k).setAttribute('aria-invalid', String(!!text));
}
function settingsUpdate() {
  const pending = settingsDiff(); let valid = true;
  for (const k of settingsKinds) {
    const pool = settingsCurrent?.pools[k], value = settingsDraft?.[k] ?? '';
    const error = pool?.editable && (!/^\d+$/.test(value) || Number(value) > 8) ? 'Enter a whole number from 0 to 8.' : '';
    settingsFieldError(k, error || settingsErrors[k] || ''); if (error) valid = false;
    el('setting-' + k).disabled = !pool?.editable || settingsSaving || settingsLoading || acting;
    setText(el('current-' + k), pool ? 'Current: ' + pool.size + ' agents' + (!pool.editable ? ' · ' + (settingsCurrent.format === 'legacy' ? 'Legacy · local conversion required' : 'Not configured') : '') : '');
    setText(el('pending-' + k), pool && value !== String(pool.size) ? 'Pending: ' + pool.size + ' → ' + (value || 'empty') : '');
  }
  const on = b => b ? 'On' : 'Off';
  el('setting-notify').disabled = !settingsCurrent || !settingsCurrent.recipient_configured || settingsSaving || settingsLoading || acting;
  setText(el('current-notify'), settingsCurrent ? 'Current: ' + on(settingsCurrent.notify_imessage_enabled) + ' · effective ' + on(settingsCurrent.notify_effective) : '');
  setText(el('pending-notify'), settingsCurrent && settingsDraft.notify !== settingsCurrent.notify_imessage_enabled ? 'Pending: ' + on(settingsCurrent.notify_imessage_enabled) + ' → ' + on(settingsDraft.notify) : '');
  settingsFieldError('notify', settingsErrors.notify || '');
  el('settingsReview').replaceChildren();
  for (const [k,value] of pending) {
    const li = node('li'), label = k === 'notify' ? 'iMessage' : k.charAt(0).toUpperCase() + k.slice(1) + ' pool';
    li.append(node('strong','',label),node('span','',k === 'notify' ? on(settingsCurrent.notify_imessage_enabled) + ' → ' + on(value) : settingsCurrent.pools[k].size + ' → ' + (value || 'empty'))); el('settingsReview').append(li);
  }
  el('settingsReviewEmpty').hidden = !!pending.length; el('settingsPending').hidden = !pending.length;
  el('settingsSave').disabled = !token || !settingsConfirmed || settingsLoading || settingsSaving || acting || !pending.length || !valid;
  el('settingsCancel').disabled = !token || !pending.length || settingsSaving || settingsLoading || acting;
  el('settingsReload').disabled = !token || settingsSaving || settingsLoading;
  setText(el('settingsSave'), settingsSaving ? 'Saving…' : 'Save changes');
  setText(el('settingsSaveStatus'), settingsSaving ? 'Save pending · Sign out stays available' : !settingsCurrent ? 'Sign in to load settings' : !pending.length ? 'No pending changes' : !valid ? 'Check the highlighted fields' : pending.length + ' pending changes');
}
function settingsResetDraft() {
  if (!settingsCurrent) return;
  settingsErrors = {};
  settingsDraft = {notify:settingsCurrent.notify_imessage_enabled};
  for (const k of settingsKinds) { settingsDraft[k] = String(settingsCurrent.pools[k].size); el('setting-' + k).value = settingsDraft[k]; }
  el('setting-notify').checked = settingsDraft.notify; settingsFieldError('notify',''); settingsUpdate();
}
function settingsReadonly(parent, label, value) {
  const field = node('div','field'), id = 'readonly-' + ++settingsReadonlyID;
  const title = node('label','',label + ' · read-only'); title.setAttribute('for',id);
  const input = node('input'); input.id = id; input.value = String(value || 'Default'); input.readOnly = true;
  input.setAttribute('readonly',''); field.append(title,input); parent.append(field);
}
function settingsReadOnlyView(c) {
  setText(el('settingsConnection'), location.href.startsWith('https:') ? 'HTTPS session' : 'Local session');
  setText(el('settingsRecipient'), c.recipient_configured ? 'Recipient configured' : 'Not configured');
  setText(el('settingsPoolHelp'), c.format === 'legacy' ? 'Legacy lanes are read-only. Run nuntius config convert locally when idle.' : 'Use whole numbers from 0 to 8. Increasing a pool does not start work.');
  const pref = el('settingsPreferences'); pref.replaceChildren();
  for (const [label,value] of [['Maximum runtime',c.max_runtime],['Resume interrupted goals',c.auto_resume?'On':'Off'],['Desktop notifications',c.notifications?'On':'Off']]) settingsReadonly(pref,label,value);
  const models = el('settingsModels'); models.replaceChildren();
  for (const l of c.lanes) {
    const lane = node('div','settings-lane'); lane.append(node('h3','',l.name));
    settingsReadonly(lane,l.kind + ' model',l.model === 'default' ? 'Default (agent configuration)' : l.model);
    settingsReadonly(lane,l.kind + ' effort',l.effort === 'default' ? 'Default (agent configuration)' : l.effort);
    if (l.kind === 'codex') lane.append(node('p','hint','Nuntius does not set Codex effort. Agent permissions stay unchanged.'));
    const details = node('details'); details.append(node('summary','','Template & execution settings · local only'));
    for (const [label,value] of [['Workspace',l.workdir],['Executable',l.binary],['Kind',l.kind],['Start mode',l.start_mode|| (l.kind==='claude'?'goal_then_continue':'as_is')],['Extra arguments',l.extra_args],['Continuation text',l.continuation_configured?'Configured locally':'Default continuation']]) details.append(node('p','',label + ': ' + value));
    lane.append(details); models.append(lane);
  }
  models.append(node('p','hint','Expanded lanes: ' + (c.lane_names.join(', ') || 'All pools disabled')));
  const machine = el('settingsMachine'); machine.replaceChildren();
  for (const [label,value] of [['Reports folder',c.reports_dir],['Oversight file',c.oversight_path || 'Default · beside reports folder'],['Start menu-bar helper at login',c.at_login],['Tailnet default',c.tailnet_default]]) settingsReadonly(machine,label,value);
  const options = node('details'); options.append(node('summary','','Startup & connection options · local only'),node('p','','Config/store locations, loopback port, allowed host, tailnet options and notification overrides are chosen locally at startup. This page cannot change or restart services.'));
  machine.append(options,node('p','hint',c.local_only_reason));
}
async function loadSettings(force = false) {
  if (!authenticated() || settingsLoading || settingsSaving || (!force && settingsReadAt && Date.now() - settingsReadAt < 10000)) return;
  settingsLoading = true; settingsReadAt = Date.now(); const epoch = authEpoch;
  settingsUpdate();
  try {
    const response = await fetch('/settings',{headers:{'X-Nuntius-Token':token},cache:'no-store'});
    if (epoch !== authEpoch || !authenticated()) return;
    if (response.status === 403) { signOut(); return; }
    if (!response.ok) throw new Error();
    const c = await response.json();
    if (epoch !== authEpoch || !authenticated()) return;
    const dirty = settingsDiff().length > 0;
    settingsCurrent = c; settingsConfirmed = true; settingsReadOnlyView(c);
    if (!dirty) settingsResetDraft();
    settingsBanner(dirty ? 'Current values refreshed. Your draft is kept; review it before saving.' : '');
  } catch {
    if (epoch !== authEpoch || !authenticated()) return;
    settingsConfirmed = false;
    settingsBanner('Settings unavailable. Your draft is kept. Reload current values before saving; check nuntius config check locally if needed.',true);
  } finally { if (epoch === authEpoch) { settingsLoading=false; settingsUpdate(); } }
}
async function saveSettings(event) {
  event?.preventDefault(); if (!authenticated()) return;
  settingsUpdate();
  const invalid = settingsKinds.find(k => el('setting-' + k).getAttribute('aria-invalid') === 'true');
  if (invalid) { el('setting-' + invalid).focus(); return; }
  if (el('settingsSave').disabled) return;
  const changes = {}; for (const [k,value] of settingsDiff()) { if (k==='notify') changes.notify_imessage_enabled=value; else { changes.pools ||= {}; changes.pools[k]=Number(value); } }
  const epoch = authEpoch; settingsSaving=true; settingsUpdate();
  try {
    const response = await fetch('/settings',{method:'POST',headers:{'X-Nuntius-Token':token,'Content-Type':'application/json'},body:JSON.stringify({schema_version:1,expected_revision:settingsCurrent.revision,changes})});
    if (epoch !== authEpoch || !authenticated()) return;
    if (response.status === 403) { signOut(); return; }
    const c = await response.json(); if (epoch !== authEpoch || !authenticated()) return;
    if (!response.ok) {
      if (response.status===409 || response.status>=500) settingsConfirmed=false;
      const messages = {busy_agent:'Nothing saved. A busy agent cannot be removed. Keep the pool large enough for running agents.',revision_conflict:'Nothing saved. Settings changed elsewhere. Reload current values and review your draft.',recipient_not_configured:'Configure an iMessage recipient locally before enabling notifications.',rate_limited:'Too many settings requests. Wait briefly, then reload current values.',committed_audit_failed:'Settings were committed, but audit completion failed. Check current values before trying again.'};
      const text = (messages[c.code] || (response.status>=500?'Could not confirm save. Check current values before trying again.':'Nothing saved. Check the highlighted fields and current configuration.')) + (c.code==='busy_agent' && /^[A-Za-z0-9][A-Za-z0-9.-]{0,63}$/.test(c.lane || '') ? ' Busy agent: ' + c.lane + '.' : '');
      settingsBanner(text,true);
      const field = c.field?.startsWith('pools.') ? c.field.slice(6) : c.field==='notify_imessage_enabled'?'notify':'';
      if ([...settingsKinds,'notify'].includes(field)) { settingsErrors[field]=text; settingsFieldError(field,text); el('setting-' + field).focus(); } else el('settingsBanner').focus();
      return;
    }
    settingsCurrent=c; settingsConfirmed=true; settingsReadOnlyView(c); settingsResetDraft(); settingsBanner('Saved. New runs use these settings.');
  } catch {
    if (epoch !== authEpoch || !authenticated()) return;
    settingsConfirmed=false; settingsBanner('Could not confirm save. Check current values before trying again.',true);
  } finally { if (epoch === authEpoch) { settingsSaving=false; settingsUpdate(); } }
}
function clearSettings() {
  settingsCurrent=null; settingsDraft=null; settingsConfirmed=false; settingsLoading=false; settingsSaving=false; settingsReadAt=0;
  settingsErrors={};
  for (const k of settingsKinds) { el('setting-'+k).value=''; settingsFieldError(k,''); }
  el('setting-notify').checked=false; settingsFieldError('notify','');
  for (const id of ['settingsPreferences','settingsModels','settingsMachine']) el(id).replaceChildren();
  for (const id of ['settingsConnection','settingsRecipient']) setText(el(id),'');
  settingsBanner(''); settingsUpdate();
}
for (const k of settingsKinds) el('setting-' + k).addEventListener('input', () => { if (!settingsDraft || settingsSaving) return; settingsDraft[k]=el('setting-'+k).value; delete settingsErrors[k]; settingsBanner(''); settingsUpdate(); });
el('setting-notify').addEventListener('change', () => { if (!settingsDraft || settingsSaving) return; settingsDraft.notify=el('setting-notify').checked; delete settingsErrors.notify; settingsBanner(''); settingsUpdate(); });
el('settingsForm').addEventListener('submit',saveSettings);
el('settingsCancel').addEventListener('click', () => { settingsResetDraft(); settingsBanner(''); });
el('settingsReload').addEventListener('click', () => loadSettings());
setInterval(() => {
  if (lastUpdate) setText(el('updated'), 'Updated ' + Math.floor((Date.now() - lastUpdate) / 1000) + 's ago');
}, 1000);
refresh();
setInterval(refresh, 2000);
