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
  cancelDashboardReads();
  clearLaneControls();
  clearSettings();
  clearQueue();
  signedOutView();
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
let laneTailView;
let laneSelection = '';
const settingsKinds = ['claude', 'codex', 'opencode'];
let settingsCurrent = null, settingsDraft = null, settingsLoading = false, settingsSaving = false;
let settingsReadAt = 0, settingsConfirmed = false;
let settingsFetched = 0;
let settingsRetry = null;
let settingsErrors = {};
let settingsReadonlyID = 0;
let queueData = null, queueFetched = 0, queueLoading = false, queueFailure = '';
const queueRows = new Map();
let laneControls = null;
const laneSaves = new Map();
const heldRows = new Map(), heldSaves = new Map(), retainedHolds = new Map();
// Match the server's active/uncertain plus terminal snapshot ceilings.
const heldReceiptLimit = 64, heldRowLimit = 128;
// One control client for Agents and future native Queue consumers. No storage.
function validLaneControls(c) {
  return c?.lane_control_version === 1 && Number.isSafeInteger(c.revision) && c.revision >= 0 &&
    Array.isArray(c.lanes) && c.lanes.length <= 64 && new Set(c.lanes.map(l => l?.name)).size === c.lanes.length &&
    c.lanes.every(l => l && typeof l.name === 'string' && /^[A-Za-z0-9][A-Za-z0-9.-]{0,63}$/.test(l.name) &&
      typeof l.paused === 'boolean' && typeof l.configured === 'boolean' && typeof l.dispatch_eligible === 'boolean' &&
      typeof l.requested_after_current === 'boolean' && Number.isSafeInteger(l.revision) && l.revision >= 0 && l.revision <= c.revision &&
      typeof l.since === 'string' && typeof l.source === 'string' && typeof l.explanation === 'string' &&
      ['enabled','paused','draining','unavailable'].includes(l.phase) &&
      (l.phase === 'unavailable' || (l.paused ? ['paused','draining'].includes(l.phase) : l.phase === 'enabled')) &&
      Array.isArray(l.blockers) && l.blockers.every(b => typeof b === 'string'));
}
function acceptLaneControls(c, version) {
  if (version !== 1 || !validLaneControls(c)) { laneControls = null; return; }
  if (!laneControls || c.revision >= laneControls.revision) laneControls = c;
  else {
    // Availability failures are observations, not older saved control values.
    // Keep the revision high-water mark and newer known lanes, but fail closed
    // for any lane unavailable or missing from this fresh observation.
    laneControls = {...laneControls, lanes:laneControls.lanes.filter(l => c.lanes.some(n => n.name === l.name)).map(l => {
      const observed = c.lanes.find(n => n.name === l.name);
      return observed.phase === 'unavailable' ? observed : l;
    })};
  }
}
function controlFor(lane) { return laneControls?.lanes.find(l => l.name === lane); }
function blockerWords(blockers) {
  const words = {lane_paused:'Lane is paused',running:'Current work is running',frozen:'Dispatch is frozen',held_message:'A held message still needs review',waiting_to_resume:'Interrupted work is waiting for a separate recovery check',not_configured:'Lane is no longer configured',freeze_unavailable:'Freeze state cannot be read; new work is blocked',held_message_unavailable:'Message reservations cannot be read; new work is blocked',lane_control_unavailable:'Lane controls are unavailable'};
  return blockers.map(b => words[b] || 'An additional hold has no explanation here').join('; ');
}
function messageGuard(lane) {
  const c = controlFor(lane);
  return !c || c.phase === 'unavailable' ? 'Lane controls are unavailable. Read current state before sending work.' :
    c.paused ? 'Lane is paused. New task messages cannot be sent; drafts, notes and reading stay available.' :
    lastData?.freeze ? 'Dispatch is frozen. New task messages cannot be sent.' : !c.configured ? 'Lane is no longer configured for new work.' : '';
}
function updateMessageControls() {
  const reason = messageGuard(el('lane').value);
  el('send').disabled = el('stopSend').disabled = !token || acting || !!reason;
  setText(el('messageHold'), reason);
}
function updateLaneControls(v, lane) {
  const c = controlFor(lane), saving = laneSaves.has(lane);
  setText(v.holdBadge, !c || c.phase === 'unavailable' ? 'Controls unavailable' : c.phase === 'draining' ? 'Paused · current run finishing' : c.paused ? 'Paused' : 'Lane enabled');
  setText(v.holdReason, !c ? 'Could not read lane holds. New starts are blocked until current state can be read.' :
    (c.paused ? 'Pause blocks new work and lets the current run finish. ' : '') + c.explanation +
    (c.since ? ' Hold saved ' + relative(c.since) + ' (' + c.since + ').' : '') +
    (c.blockers.length ? ' Holds: ' + blockerWords(c.blockers) + '.' : ''));
  // Do not hide the button that owns keyboard focus when its set completes.
  v.pause.hidden = !!c?.paused && document.activeElement !== v.pause;
  v.pauseAfter.hidden = !!c?.paused && document.activeElement !== v.pauseAfter;
  v.resume.hidden = !!c && !c.paused && document.activeElement !== v.resume;
  for (const b of [v.pause,v.pauseAfter,v.resume]) b.disabled = !token || saving || !c || c.phase === 'unavailable';
  v.pause.disabled ||= !c?.configured;
  v.pauseAfter.disabled ||= !c?.configured;
}
function clearLaneControls() {
  for (const request of laneSaves.values()) request.cancel();
  laneSaves.clear(); laneControls = null;
  for (const request of heldSaves.values()) request.cancel();
  heldSaves.clear();
}
async function setLaneControl(lane, operation, afterCurrent = false) {
  if (!authenticated() || laneSaves.has(lane)) return;
  const c = controlFor(lane), v = cards.get(lane) || retainedHolds.get(lane);
  const retainedResume = operation === 'resume' && retainedHolds.get(lane) === v && c && !c.configured && !c.blockers.includes('lane_control_unavailable');
  if (!v || !c || (c.phase === 'unavailable' && !retainedResume) || (operation === 'pause' && !c.configured)) return;
  const epoch = authEpoch, controller = new AbortController();
  let rejectSave;
  const cancelled = new Promise((_, reject) => { rejectSave = reject; });
  const request = {cancel: () => { controller.abort(); rejectSave(new Error('save unconfirmed')); }};
  const current = () => laneSaves.get(lane) === request && epoch === authEpoch && authenticated();
  laneSaves.set(lane, request);
  setText(v.controlStatus, 'Save pending · current hold is not yet confirmed. Sign out stays available.');
  if (cards.get(lane) === v) updateLaneControls(v, lane); else updateRetainedHold(v, c);
  const payload = {schema_version:1,lane,operation,expected_revision:laneControls.revision};
  if (operation === 'pause') payload.after_current = afterCurrent;
  const timer = setTimeout(request.cancel, 10000);
  let reread = false;
  try {
    const {response, data} = await Promise.race([cancelled, (async () => {
      const response = await fetch('/lane-control', {method:'POST',headers:{'X-Nuntius-Token':token,'Content-Type':'application/json'},body:JSON.stringify(payload),signal:controller.signal});
      if (!current()) return {};
      if (response.status === 403) return {response};
      return {response, data:await response.json()};
    })()]);
    if (!current()) return;
    if (response.status === 403) { signOut(); return; }
    if (!response.ok || data?.schema_version !== 1 || typeof data.paused !== 'boolean' || data.paused !== (operation === 'pause') || typeof data.changed !== 'boolean' ||
        !validLaneControls(data.lane_control) || data.revision !== data.lane_control.revision ||
        data.lane_control.lanes.find(l => l.name === lane)?.paused !== data.paused) {
      const messages = {revision_conflict:'Lane controls changed elsewhere. Current state is being read; review it before another action.',locked:'Lane controls are busy. No change was accepted. Read current state before another action.',rate_limited:'Too many control requests. Read current state before another action.',invalid_request:'This control request was refused. Read current state and check local configuration.'};
      setText(v.controlStatus, messages[data?.code] || 'Save not confirmed. The hold may have changed. Reading current state; review it before another action.');
      laneControls = null; reread = true;
    } else if (data.revision < Math.max(payload.expected_revision, laneControls?.revision ?? 0)) {
      acceptLaneControls(data.lane_control, 1);
      setText(v.controlStatus, 'A newer lane-control change exists. Reading current state; review it before another action.');
      reread = true;
    } else {
      acceptLaneControls(data.lane_control, 1);
      const saved = controlFor(lane);
      setText(v.controlStatus, operation === 'pause' ?
        (data.changed ? 'Lane hold saved.' : 'This lane was already paused.') + ' New starts are blocked; current work can finish.' :
        (data.changed ? 'Lane hold cleared;' : 'This lane was already resumed;') + ' this action started no work. Other holds: ' + (saved.blockers.length ? blockerWords(saved.blockers) : 'none in this snapshot') + '. A previously approved automatic batch may start work later. Held messages still need explicit review.');
    }
  } catch {
    if (!current()) return;
    laneControls = null; reread = true;
    setText(v.controlStatus, 'Save not confirmed. The hold may have changed. Reading current state; review it before another action.');
  } finally {
    clearTimeout(timer);
    if (laneSaves.get(lane) === request) {
      laneSaves.delete(lane);
      if (epoch === authEpoch && authenticated()) {
        for (const [name,view] of cards) updateLaneControls(view, name);
        renderRetainedHolds();
        updateHeldControls();
        updateMessageControls();
      }
    }
  }
  if (reread && epoch === authEpoch && authenticated()) await refresh(true);
}

function makeRetainedHold(c) {
  const root = node('article', 'held-card'), name = node('h4', '', c.name);
  const reason = node('p', 'hint'), controlStatus = node('p', 'hint');
  controlStatus.setAttribute('role', 'status');
  const resume = button('Resume retained lane', '', () => setLaneControl(c.name, 'resume'));
  root.append(name, reason, resume, controlStatus);
  return {root, reason, resume, controlStatus};
}
function updateRetainedHold(v, c) {
  setText(v.reason, c.paused ? 'Paused · no longer configured. ' + blockerWords(c.blockers) : 'Hold cleared · still not configured. This action started no work.');
  v.resume.disabled = !token || laneSaves.has(c.name) || !c.paused || c.blockers.includes('lane_control_unavailable');
}
function renderRetainedHolds() {
  const entries = (laneControls?.lanes || []).filter(c => !c.configured && (c.paused || retainedHolds.has(c.name)));
  reconcile(el('retainedHolds'), entries, retainedHolds, makeRetainedHold, updateRetainedHold, c => c.name);
  for (const [name,v] of retainedHolds) if (!entries.some(c => c.name === name)) {
    v.resume.disabled = true;
    setText(v.reason, 'Current state for this retained hold is unavailable. Read again before another action.');
  }
  el('retainedSection').hidden = el('retainedHolds').children.length === 0;
}
function validHeldSnapshot(value) {
  return value?.schema_version === 1 && value.status === 'ok' && typeof value.older_receipts_omitted === 'boolean' && Array.isArray(value.messages) && value.messages.length <= 128 &&
    new Set(value.messages.map(m => m?.lane + ':' + m?.run_id)).size === value.messages.length && value.messages.every(m =>
      m && typeof m.lane === 'string' && /^[A-Za-z0-9][A-Za-z0-9.-]{0,63}$/.test(m.lane) && typeof m.run_id === 'string' && /^[a-f0-9]{32}$/.test(m.run_id) && typeof m.goal_id === 'string' &&
      Number.isSafeInteger(m.content_revision) && m.content_revision > 0 && typeof m.content_sha256 === 'string' && /^[a-f0-9]{64}$/.test(m.content_sha256) &&
      typeof m.text === 'string' && m.text.length <= 16384 && ['held_for_review','reserved','consumed','discarded'].includes(m.state) &&
      typeof m.session_available === 'boolean' && typeof m.delivery_confirmed === 'boolean' && typeof m.content_discarded === 'boolean');
}
function heldIdentity(m) { return m.lane + ':' + m.run_id; }
function sameHeldContent(a, b) {
  return a && b && heldIdentity(a) === heldIdentity(b) && a.goal_id === b.goal_id && a.content_revision === b.content_revision && a.content_sha256 === b.content_sha256 && a.text === b.text;
}
function heldSendReason(m) {
  const c = controlFor(m.lane);
  if (!c || c.phase === 'unavailable') return 'Lane controls are unavailable. Read current state before sending.';
  if (c.paused) return 'Lane is paused. Resume clears only the lane hold; the message still needs review.';
  if (lastData?.freeze) return 'Dispatch is frozen. Sending is blocked; reading and discard remain available.';
  if (!c.configured) return 'Lane is no longer configured. Sending is blocked.';
  if (!m.session_available) return 'The original session is unavailable. Sending is blocked.';
  const blockers = c.blockers.filter(b => b !== 'held_message');
  return blockers.length ? blockerWords(blockers) + '. Sending is blocked.' : '';
}
function makeHeldRow(m) {
  const root = node('article', 'held-card'), title = node('h4'), state = node('p', 'hint'), guard = node('p', 'hint');
  const review = node('details'), summary = node('summary', '', 'Review the held message and original identity');
  const text = node('pre', 'report-text'), identity = node('p', 'hint');
  const label = node('label', 'follow'), confirm = node('input'); confirm.type = 'checkbox';
  label.append(confirm, node('span', '', 'I reviewed this exact message for its original goal.'));
  const send = button('Send reviewed message', 'primary', () => mutateHeld(heldIdentity(m), 'send-held'));
  const discard = button('Discard reviewed message', '', () => mutateHeld(heldIdentity(m), 'discard-held'));
  const actions = node('div', 'actions'); actions.append(send, discard);
  const status = node('p', 'hint'); status.setAttribute('role', 'status');
  review.append(summary, identity, text, label); root.append(title, state, review, guard, actions, status);
  const v = {root,title,state,guard,review,text,identity,confirm,send,discard,status,data:m};
  confirm.addEventListener('change', () => updateHeldRow(v, v.data));
  review.addEventListener('toggle', () => updateHeldRow(v, v.data));
  return v;
}
function updateHeldRow(v, m) {
  if (!sameHeldContent(v.data, m)) v.confirm.checked = false;
  v.data = m;
  setText(v.title, m.lane + ' · Original goal: ' + m.goal_id);
  setText(v.identity, 'Original run: ' + m.run_id + ' · Content revision: ' + m.content_revision + ' · Review digest: ' + m.content_sha256);
  setText(v.text, m.text || 'Message content is no longer retained with this receipt.');
  setText(v.state, m.content_discarded && m.state === 'reserved' ? 'Reviewed content discarded. A possible follow-up run still needs review; discard does not cancel it.' : m.state === 'held_for_review' ? 'Held message needs your review. Resume never sends it automatically.' :
    m.state === 'discarded' ? 'Discarded · durable receipt for this reviewed message.' :
    m.state === 'consumed' && m.delivery_confirmed ? 'Sent to a follow-up run · durable receipt and original run identity confirmed. This does not confirm successful work.' :
    'Delivery is not confirmed. A reservation or receipt needs a separate review; do not send again automatically.');
  const snapshot = lastData?.held_messages;
  const present = validHeldSnapshot(snapshot) && snapshot.messages.some(current => sameHeldContent(current, m) && current.state === m.state);
  if (!present) setText(v.state, 'Last-known review state: ' + v.state.textContent);
  const pending = heldSaves.has(heldIdentity(m)), reason = heldSendReason(m);
  setText(v.guard, present ? reason : 'Current held-message state is unavailable. Read again before another action.');
  const reviewRequired = !token || pending || !present || !v.review.open || !v.confirm.checked;
  v.send.disabled = acting || reviewRequired || m.state !== 'held_for_review' || !!reason;
  v.discard.disabled = acting || reviewRequired || !['held_for_review','reserved'].includes(m.state) || !!m.content_discarded;
}
function updateHeldControls() {
  for (const v of heldRows.values()) updateHeldRow(v, v.data);
}
function renderHeldMessages(d) {
  const value = d.held_messages;
  if (!validHeldSnapshot(value)) {
    setText(el('heldSummary'), 'Could not read held messages or receipts. Last-known messages stay visible; no delivery is confirmed.');
    updateHeldControls(); return;
  }
  const terminal = m => m.state === 'discarded' || (m.state === 'consumed' && m.delivery_confirmed);
  const received = new Map(value.messages.map(m => [heldIdentity(m), m])), selected = new Map();
  const previous = Array.from(heldRows.values());
  const priority = v => v.root.contains(document.activeElement) ? 0 : heldSaves.has(heldIdentity(v.data)) ? 1 : busy(v.root) ? 2 : 3;
  const pinned = previous.filter(v => heldSaves.has(heldIdentity(v.data)) || busy(v.root) || v.review.open).sort((a,b) => priority(a) - priority(b));
  let receipts = 0;
  const keep = m => {
    const key = heldIdentity(m);
    if (selected.has(key) || selected.size >= heldRowLimit || (terminal(m) && receipts >= heldReceiptLimit)) return;
    selected.set(key, m); if (terminal(m)) receipts++;
  };
  // Current active/uncertain reservations always win over receipt history.
  value.messages.filter(m => !terminal(m)).forEach(keep);
  // Keep only the specific reviews being read/acted on, never their old peers.
  // Focus and pending actions take precedence if many old reviews remain open.
  pinned.forEach(v => keep(received.get(heldIdentity(v.data)) || v.data));
  value.messages.filter(terminal).forEach(keep);
  const omitted = value.older_receipts_omitted || value.messages.some(m => !selected.has(heldIdentity(m))) || previous.some(v => !selected.has(heldIdentity(v.data)));
  const latest = value.messages.filter(m => terminal(m) && selected.has(heldIdentity(m))).length;
  const earlier = Array.from(selected.keys()).filter(key => !received.has(key)).length;
  setText(el('heldSummary'), (value.messages.length ? 'Review the original goal and message before Send or Discard. Resume never replays a held message.' : 'No held messages or receipts in this received snapshot.') +
    (omitted ? ' Showing the latest ' + latest + ' receipts from this snapshot. Other receipts are omitted to keep this view bounded.' : '') +
    (earlier ? ' ' + earlier + ' earlier reviews stay visible with last-known state; read again before another action.' : ''));
  let entries = Array.from(selected.values());
  if (pinned.length) {
    // Preserve the place and nodes of retained reviews while replacing peers.
    entries = previous.filter(v => selected.has(heldIdentity(v.data))).map(v => selected.get(heldIdentity(v.data)))
      .concat(entries.filter(m => !heldRows.has(heldIdentity(m))));
  }
  // Generic reconciliation may keep a recently interacted-with row. This view
  // has already budgeted those rows, so it must enforce its own hard ceiling.
  for (const [key,v] of heldRows) if (!selected.has(key)) { v.root.remove(); heldRows.delete(key); }
  reconcile(el('heldMessages'), entries, heldRows, makeHeldRow, updateHeldRow, heldIdentity);
  // A disappeared row that owns focus stays readable but cannot be acted on.
  updateHeldControls();
}
async function mutateHeld(key, operation) {
  if (!authenticated() || heldSaves.has(key) || !['send-held','discard-held'].includes(operation)) return;
  const v = heldRows.get(key), snapshot = lastData?.held_messages;
  if (!v || !validHeldSnapshot(snapshot)) return;
  const m = snapshot.messages.find(m => heldIdentity(m) === key);
  if (!sameHeldContent(v.data, m) || !v.review.open || !v.confirm.checked || acting ||
      (operation === 'send-held' ? m.state !== 'held_for_review' || heldSendReason(m) : !['held_for_review','reserved'].includes(m.state) || m.content_discarded)) return;
  const epoch = authEpoch, controller = new AbortController(); let rejectSave;
  const cancelled = new Promise((_, reject) => { rejectSave = reject; });
  const request = {cancel: () => { controller.abort(); rejectSave(new Error('held action unconfirmed')); }};
  const current = () => heldSaves.get(key) === request && epoch === authEpoch && authenticated();
  const payload = {schema_version:1,lane:m.lane,run_id:m.run_id,content_revision:m.content_revision,content_sha256:m.content_sha256,operation};
  heldSaves.set(key, request); updateHeldControls();
  setText(v.status, 'Action pending. This held message has not been confirmed sent or discarded. Sign out stays available.');
  const timer = setTimeout(request.cancel, 10000);
  try {
    const {response, data} = await Promise.race([cancelled, (async () => {
      const response = await fetch('/held-message', {method:'POST',headers:{'X-Nuntius-Token':token,'Content-Type':'application/json'},body:JSON.stringify(payload),signal:controller.signal});
      if (!current()) return {};
      if (response.status === 403) return {response};
      return {response, data:await response.json()};
    })()]);
    if (!current()) return;
    if (response.status === 403) { signOut(); return; }
    const valid = response.ok && data?.schema_version === 1 && data.operation === operation && /^[a-f0-9]{32}$/.test(data.run_id) && (operation !== 'discard-held' || data.run_id === m.run_id);
    setText(v.status, valid ? 'The reviewed action was accepted. Reading its durable receipt before confirming the result.' :
      data?.code === 'revision_conflict' ? 'The reviewed message changed. Nothing is confirmed; read its current identity and review again.' :
      'Action not confirmed. The message may have changed. Reading its receipt; do not retry automatically.');
  } catch {
    if (!current()) return;
    setText(v.status, 'Action not confirmed. The message may have changed. Reading its receipt; do not retry automatically.');
  } finally {
    clearTimeout(timer);
    if (heldSaves.get(key) === request) {
      heldSaves.delete(key); v.confirm.checked = false;
      if (epoch === authEpoch && authenticated()) updateHeldControls();
    }
  }
  if (epoch === authEpoch && authenticated()) await refresh(true);
}
// Snapshot GETs only. Event streams and mutation POSTs have separate lifetimes.
// Explicit settlement also handles a transport that fails to honor abort.
const dashboardReadDeadline = 10000;
const dashboardReads = new Map();
let dashboardReadID = 0, pageActive = true, foregroundQueued = false;
function beginDashboardRead(path, replace = false) {
  const previous = dashboardReads.get(path);
  if (previous && !replace) return null;
  if (previous) previous.cancel();
  const controller = new AbortController(), epoch = authEpoch;
  let rejectRead;
  const cancelled = new Promise((_, reject) => { rejectRead = reject; });
  const request = {
    id: ++dashboardReadID, epoch, controller,
    current: () => dashboardReads.get(path) === request && epoch === authEpoch && authenticated(),
    cancel: () => { controller.abort(); rejectRead(new Error('read cancelled')); },
    json: async () => {
      const timer = setTimeout(request.cancel, dashboardReadDeadline);
      try {
        return await Promise.race([cancelled, (async () => {
          const response = await fetch(path, {headers:{'X-Nuntius-Token':token}, cache:'no-store', signal:controller.signal});
          // Revocation does not require a decodable body.
          const data = response.status === 403 ? null : await response.json();
          return {response, data};
        })()]);
      } finally { clearTimeout(timer); }
    },
    finish: () => {
      if (dashboardReads.get(path) !== request) return false;
      dashboardReads.delete(path); return true;
    }
  };
  dashboardReads.set(path, request);
  return request;
}
function cancelDashboardReads() {
  for (const request of dashboardReads.values()) request.cancel();
  dashboardReads.clear();
  refreshing = false; queueLoading = false; settingsLoading = false;
  clearTimeout(settingsRetry); settingsRetry = null;
}
function foregroundRefresh() {
  if (document.hidden) return;
  pageActive = true;
  if (foregroundQueued) return;
  foregroundQueued = true;
  Promise.resolve().then(() => {
    foregroundQueued = false;
    if (!pageActive || document.hidden || !authenticated()) return;
    refresh(true);
    if (!el('queue').hidden) loadQueue(true);
    if (!el('settings').hidden) loadSettings(true);
  });
}
const auditReadsKey = 'nuntius.web.audit.showReads';
try {
  el('auditReads').checked = localStorage.getItem(auditReadsKey) === 'true';
} catch { /* Filtering still works when browser storage is disabled. */ }

function selectTab(id, focus = false, push = true) {
  const signedIn = authenticated();
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
    panel.hidden = !selected || !signedIn;
  }
  const panelID = active.getAttribute('aria-controls');
  const hash = '#' + panelID + (panelID === 'agents' && laneSelection ? '/' + laneSelection : '');
  if (location.hash !== hash) history[push ? 'pushState' : 'replaceState'](null, '', '/' + hash);
  if (focus) active.focus();
  el('signInPanel').hidden = signedIn;
  el('overview').hidden = !signedIn;
  if (!signedIn) signedOutView();
  if (lastData) renderPanels(lastData, shown);
  if (panelID === 'settings') {
    active.scrollIntoView?.({block:'nearest', inline:'nearest'});
    loadSettings();
  }
  if (panelID === 'queue') loadQueue();
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
  const health = node('h4', 'lane-health'), reason = node('p', 'lane-reason'), rankReason = node('p', 'hint');
  const holdBadge = node('span', 'badge lane-hold'), holdReason = node('p', 'hint');
  const durationBasis = node('p', 'duration-basis'), received = node('p', 'hint');
  const reportStatus = node('p', 'hint'), reportLink = node('a', 'lane-report');
  reportLink.href = '#runs';
  reportLink.addEventListener('click', event => {
    event.preventDefault();
    const r = view.reportRun;
    if (!r) return;
    selectTab('runs');
    el('filter').value = ''; filterRuns();
    const target = runs.get(r.run_id);
    if (target) { target.detail.hidden = false; target.toggle.setAttribute('aria-expanded','true'); updateRunContent(target,target.data); target.toggle.focus(); }
  });
  const tail = node('details'), tailText = node('pre', 'log live-tail');
  const tailAge = node('p', 'tail-age'), tailNotice = node('p', 'muted');
  tailText.tabIndex = 0;
  const {tailState, tailJump} = bindTail(tailText);
  tail.addEventListener('toggle', () => {
    if (tail.open && tailState.following) tailText.scrollTop = tailText.scrollHeight;
  });
  tail.append(node('summary', '', 'View tail'), tailAge, tailJump, tailNotice, tailText);
  const controls = node('div', 'card-actions'), cancelControls = node('span');
  const message = button('Messages & notes', '', () => {
    selectTab('messages');
    el('lane').value = s.lane;
    updateMessageControls();
    el('message').focus();
  });
  const pause = button('Pause lane', '', () => setLaneControl(s.lane, 'pause', false));
  const pauseAfter = button('Pause after current', '', () => setLaneControl(s.lane, 'pause', true));
  const resume = button('Resume lane', '', () => setLaneControl(s.lane, 'resume'));
  const controlStatus = node('p','control-status'); controlStatus.setAttribute('role','status');
  controls.append(pause, pauseAfter, resume, message);
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
  const evidence = node('details', 'lane-evidence');
  evidence.append(node('summary','','Evidence and exact times'), stats);
  root.append(head, goal, health, reason, rankReason, activity, durationBasis, received, reportStatus, reportLink, holdBadge, holdReason, controls, controlStatus, tail, confirm, evidence);
  const view = {root, kind, badge, goal, fields, health, reason, rankReason, holdBadge, holdReason, durationBasis, received, reportStatus, reportLink, evidence, pause, pauseAfter, resume, message, controlStatus, activityText, tail, tailText, tailAge, tailJump, tailNotice, tailState, cancelControls, confirm, resetCancel};
  return view;
}
function bindTail(tailText) {
  const tailState = {lines: [], run: '', following: true, pending: 0};
  const tailJump = button('Jump to latest', 'tail-jump', () => {
    tailState.following = true; tailState.pending = 0; tailJump.hidden = true;
    tailText.scrollTop = tailText.scrollHeight;
  });
  tailJump.hidden = true;
  tailText.addEventListener('scroll', () => {
    tailState.following = tailText.scrollHeight - tailText.clientHeight - tailText.scrollTop <= 16;
    if (tailState.following) { tailState.pending = 0; tailJump.hidden = true; }
  });
  return {tailState, tailJump};
}
// Transport-independent bounded snapshot consumer. B21 can call this on resync.
// Keep overlapping line nodes so opening/focusing the tail does not freeze it.
function updateCardTail(v, s, received) {
  const state = v.tailState;
  const lines = received.flatMap(line => String(line).split('\n')).slice(-500);
  const run = (s.lane || '') + ':' + (s.run_id || '');
  const sameRun = state.run === run;
  let overlap = 0;
  if (sameRun) for (let n = Math.min(state.lines.length, lines.length); n > 0; n--) {
    if (state.lines.slice(-n).every((line, i) => line === lines[i])) { overlap = n; break; }
  }
  const added = lines.length - overlap;
  const oldTop = v.tailText.scrollTop || 0, oldHeight = v.tailText.scrollHeight || 0;
  if (!sameRun || (state.lines.length && !overlap)) {
    v.tailText.replaceChildren();
    state.pending = 0;
    setText(v.tailNotice, sameRun ? 'Tail window changed; older lines are unavailable here.' : 'Latest bounded output for this attempt.');
  } else {
    const removed = state.lines.length - overlap;
    for (let i = 0; i < removed; i++) v.tailText.firstElementChild?.remove();
  }
  if (added || !lines.length) {
    if (!state.lines.length) v.tailText.replaceChildren();
    for (let i = overlap; i < lines.length; i++) {
      if (v.tailText.firstElementChild) v.tailText.children[v.tailText.children.length - 1].textContent += '\n';
      v.tailText.append(node('span', '', lines[i]));
    }
    if (!lines.length) setText(v.tailText, 'No output yet.');
    if (v.tail.open && state.following) v.tailText.scrollTop = v.tailText.scrollHeight;
    else if (v.tail.open) {
      state.pending += added;
      // Keep the owner's place when the bounded window evicts old lines.
      v.tailText.scrollTop = Math.max(0, oldTop - Math.max(0, oldHeight - v.tailText.scrollHeight));
    }
  }
  state.run = run; state.lines = lines;
  v.tailJump.hidden = !state.pending;
  setText(v.tailJump, state.pending + ' new lines - jump to latest');
  v.tailObservedAge = s.last_event_age_seconds; v.tailReceived = lastUpdate || Date.now();
  updateTailAge(v);
}
function updateTailAge(v) {
  setText(v.tailAge, v.tailState.lines.length && v.tailObservedAge != null ? 'last line ' + Math.floor(v.tailObservedAge + Math.max(0, Date.now() - v.tailReceived) / 1000) + 's ago · source: last observed event' : 'Last line age not reported');
}
function updateCard(v, s) {
  updateCardTail(v, s, (lastData.tails || {})[s.lane] || []);
  updateLaneControls(v, s.lane);
  if (busy(v.root)) return;
  setText(v.kind, (s.kind || 'Agent') + ' · model=' + (s.model || 'default') + ' effort=' + (s.effort || 'default'));
  stateBadge(v.badge, s.display_state || s.state, s.badge);
  v.goal.className = s.goal_id ? 'goal' : 'goal goal-placeholder';
  setText(v.goal, s.goal_id || 'No current goal');
  const explanation = laneExplanation(s);
  setText(v.health, explanation.label); setText(v.reason, explanation.reason);
  setText(v.rankReason, 'Order reason: ' + explanation.rankReason);
  setText(v.fields.Elapsed, s.goal_id ? duration(s.elapsed_seconds) : '—');
  setText(v.fields.Run, s.run_id || '—');
  v.fields.Elapsed.title = s.started ? 'Started ' + new Date(s.started).toLocaleString() : '';
  setText(v.fields.ETA, s.eta === '-' ? 'Not enough history' : s.eta || '—');
  const etaExplanation = s.eta_details?.explanation || '';
  v.fields.ETA.title = etaExplanation;
  v.fields.ETA.tabIndex = etaExplanation ? 0 : -1;
  v.fields.ETA.onclick = () => { if (etaExplanation) alert(etaExplanation); };
  const e = s.eta_details;
  setText(v.durationBasis, (s.state === 'running' ? 'Elapsed: ' : 'Run duration: ') + (s.goal_id ? duration(s.elapsed_seconds) : 'No run') +
    (e && Number.isFinite(e.median_seconds) && Number.isSafeInteger(e.samples) && e.samples > 0 && e.kind && e.size ?
      ' / usual ' + duration(e.median_seconds) + '. Usual is the median of ' + e.samples + ' earlier ' + e.kind + ', size ' + e.size + ' runs; this estimate does not promise completion. ' + etaExplanation : ' · usual duration unavailable. ' + etaExplanation));
  v.cardReceived = lastUpdate || Date.now(); v.eventAge = s.last_event_age_seconds;
  updateCardAge(v);
  setText(v.fields['Last event'], s.last_event_age_seconds == null ? '—' : duration(s.last_event_age_seconds) + ' ago');
  v.fields['Last event'].title = s.last_event ? new Date(s.last_event).toLocaleString() : '';
  setText(v.fields.Report, s.state === 'idle' ? 'No current attempt.' : s.report_exists ? 'Goal-level report exists; see attempt association above.' : 'No report available for the current attempt.');
  const history = lastData.history || [];
  const current = history.find(r => r.lane === s.lane && r.run_id === s.run_id && r.report && r.report_association?.startsWith('Associated by report time'));
  const previous = history.find(r => r.lane === s.lane && r.run_id !== s.run_id && r.report && r.report_association?.startsWith('Associated by report time'));
  v.reportRun = current || previous;
  v.reportLink.hidden = !v.reportRun;
  setText(v.reportLink, v.reportRun ? (current ? 'Current attempt report: Read ' : 'Previous goal report: Read ') + v.reportRun.goal_id + ' report — ended ' + relative(v.reportRun.ended) : '');
  setText(v.reportStatus, current ? 'Report exists for this received attempt; acceptance is not confirmed.' : s.state === 'running' ? 'Current attempt has no report yet.' : s.report_exists ? 'A goal-level report exists, but its association with this attempt is unavailable in the received history.' :
    s.state === 'idle' ? 'No current attempt. Earlier reports outside the latest received history are not known here.' : 'Current attempt has no report. Inspect the retained tail.');
  const a = s.current_action;
  const estimate = a && a.age_source !== 'event' ? '~' : '';
  setText(v.activityText, a ? a.summary + ' (' + estimate + a.age_seconds + 's ago)' +
    (a.running_seconds == null ? '' : '; running for ' + estimate + a.running_seconds + 's') +
    (a.age_source === 'event' ? ' · age from event timestamps' : ' · age estimated from observation') :
    s.activity === '-' ? 'No recent activity' : s.activity || 'No recent activity');
  v.activityText.title = v.activityText.textContent;
  v.cancelControls.hidden = s.state !== 'running';
  if (s.state !== 'running') v.resetCancel();
}
function updateCardAge(v) {
  if (!v.cardReceived) return;
  const age = Math.max(0,Date.now()-v.cardReceived)/1000;
  setText(v.received, 'Last activity: ' + (v.eventAge == null ? 'age not reported' : duration(v.eventAge+age) + ' ago') +
    ' · operational data received ' + Math.floor(age) + 's ago. Receipt is not proof of progress.');
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
  const chain = node('p', 'report-meta'), age = node('p', 'report-meta');
  const warnings = node('div', 'report-warnings'), ownerNeed = node('p', 'report-attention'), processOutcome = node('p');
  content.append(chain, age, warnings, ownerNeed, processOutcome);
  const sections = {};
  for (const [name, title] of [['happened','What happened'], ['QUESTIONS','Does it need me?'], ['commits','Commits and pushes'], ['OPEN','What is next']]) {
    const box = node('section', 'report-block'), pre = node('pre', 'report-text');
    box.append(node('h3', '', title), pre); content.append(box); sections[name] = pre;
  }
  const decisions = node('details'), decisionText = node('pre', 'report-text');
  decisions.append(node('summary', '', 'Reported decisions — supporting context'), decisionText); content.append(decisions);
  const goalText = node('pre', 'report-text'), report = node('pre', 'report-text');
  const rawReport = node('details', 'report-block'), rawGoal = node('details', 'report-block');
  rawReport.append(node('summary', '', 'Read full report (redacted text)'), report);
  rawGoal.append(node('summary', '', 'Read original goal (redacted text)'), goalText);
  content.append(rawReport, rawGoal);
  detail.append(content); detail.hidden = true; root.append(row, detail);
  const view = {root, lane, runID, relation, identity, reportLink, toggle, badge, timestamp, elapsed, detail, sections, chain, age, warnings, ownerNeed, processOutcome, decisionText, rawReport, rawGoal, goalText, report, data: r};
  return view;
}
function updateRunContent(v, r) {
  const p = r.overview;
  const blocks = name => (p?.blocks?.[name] || []).join('\n');
  setText(v.chain, [r.goal_id, r.lane, 'Attempt ' + r.run_id, r.relation || 'first run', r.report_association || 'Attempt association not reported'].join(' · '));
  updateRunAge(v, r);
  const messages = [...(p?.warnings || [])];
  if (p?.incomplete) messages.unshift('Incomplete interpretation — inspect the available redacted report.');
  if (p?.status_conflict) messages.unshift('Conflicting report status');
  if (!p) messages.push('Report overview not available; inspect the full redacted text.');
  if ((r.goal || '').includes('[truncated]') || (r.goal || '').includes('[REDACTED]')) messages.push('Original goal text is truncated or redacted.');
  setText(v.warnings, messages.join('\n'));
  v.warnings.hidden = !messages.length;
  const owner = p?.owner_need === 'decision' ? 'Needs your decision' : p?.owner_need === 'none' ? 'No owner decision reported' : 'Owner need unknown — inspect available report';
  setText(v.ownerNeed, owner + (p?.attention && p?.owner_need === 'none' ? ' · Report status or other sections need attention' : ''));
  const process = r.state === 'running' ? 'Process is running' : r.ended && r.exit === 0 ? 'Process ended normally' : r.ended && r.exit != null ? 'Process ended with exit ' + r.exit : 'Process outcome unknown';
  setText(v.processOutcome, process + '; ' + (p?.status_conflict ? 'Conflicting report status' : blocks('STATUS') ? 'report says ' + blocks('STATUS').trimEnd() : 'report status not supplied') + '. Process outcome does not establish acceptance or verification.');
  setText(v.sections.happened, 'Report says\n' + (blocks('PLAIN SUMMARY') || 'No plain summary supplied\n') + '\nSTATUS\n' + (blocks('STATUS') || 'Not reported'));
  setText(v.sections.QUESTIONS, (blocks('QUESTIONS') || owner) + (p?.owner_need === 'unknown' && blocks('QUESTIONS') ? '\n' + owner : ''));
  setText(v.sections.commits, 'Reported, not independently checked\nIDS\n' + (blocks('IDS') || 'Not reported\n') + '\nHEADS\n' + (blocks('HEADS') || 'Not reported'));
  setText(v.sections.OPEN, (blocks('OPEN') || 'No next step reported\n') + (p?.owner_need !== 'none' && blocks('QUESTIONS') ? '\nReported questions remain:\n' + blocks('QUESTIONS') : ''));
  setText(v.decisionText, blocks('DECISIONS') || 'Not reported');
  setText(v.goalText, r.goal || 'No goal text available.');
  setText(v.report, r.report || 'No report available.');
}
function updateRunAge(v, r) {
  setText(v.age, 'Ended ' + (r.ended ? relative(r.ended) : 'time not reported') + ' · Received ' + (lastUpdate ? Math.floor((Date.now() - lastUpdate) / 1000) + 's ago' : 'this update') + ' · Report text stays as opened while you read.');
  v.age.title = r.ended ? 'Ended local: ' + new Date(r.ended).toLocaleString() + ' · UTC: ' + new Date(r.ended).toISOString() : '';
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
  if (!v.detail.hidden) updateRunAge(v, r);
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
  setText(el('historyBound'), 'Showing ' + visible + ' matching attempts from the latest ' + (lastData?.history.length || 0) + ' received (up to 50); older runs are not in this view. Attention counts received attempts; resumed reports can repeat questions.');
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
  if (state === 'stuck?' || (s.state === 'running' && (s.last_event_age_seconds > 900 || (!s.last_event && s.elapsed_seconds > 900)))) return {kind:'stuck', label:'Possible stall — silence may be a long command'};
  if (/^(ended|completed)/.test(state)) return {kind:'finished', label:'Finished'};
  if (/^running/.test(state)) return {kind:'running', label:'Running'};
  return {kind:'idle', label:'Idle'};
}
function laneExplanation(s) {
  const c = controlFor(s.lane), mark = laneIndicator(s);
  const r = (lastData.history || []).find(r => r.lane === s.lane && r.run_id === s.run_id);
  if (mark.kind === 'failed' || mark.kind === 'attention') return {rank:0,rankReason:'Failed or missing-report attempt needs review',label:'Ended — needs review',reason:(s.exit != null ? 'Process exit ' + s.exit + '. ' : 'Process outcome needs inspection. ') + (s.report_exists ? 'Read the report; acceptance is not confirmed.' : 'Report is missing. Inspect the retained tail to find what happened.')};
  if (s.state !== 'running' && r?.overview?.attention) return {rank:0,rankReason:'Report needs attention',label:'Ended — needs review',reason:r.overview.owner_need === 'decision' ? 'The report includes an owner decision. Read its options and recommendation.' : 'The report status or missing interpretation needs review.'};
  if (mark.kind === 'stuck') return {rank:1,rankReason:'No action observed past the 15-minute silence threshold',label:'Possible stall',reason:'No newer action observed for ' + duration(s.last_event_age_seconds ?? s.elapsed_seconds) + '. This may be a long command. Inspect the tail before deciding; silence does not confirm a stuck process.'};
  if (!c || c.phase === 'unavailable') return {rank:1,rankReason:'Current lane controls are unavailable',label:s.state === 'running'?'Working — controls unavailable':'Lane controls unavailable',reason:'New-start permission is unknown and blocked. Tail, reports, notes and independent safety controls remain usable.'};
  if (s.state !== 'running' && s.state !== 'idle') return {rank:2,rankReason:'Ended attempt awaits review',label:'Ended — awaiting review',reason:'Process ended' + (s.exit === 0 ? ' normally' : s.exit == null ? ' with unknown exit' : ' with exit ' + s.exit) + '. A saved report does not establish acceptance or verification.'};
  if (s.state === 'running') return {rank:3,rankReason:'Current work is running',label:s.current_action?.running_seconds != null?'Waiting on its own work':'Working',reason:s.current_action?.running_seconds != null ? 'Waiting for its command to finish; running for ' + duration(s.current_action.running_seconds) + '. This is observed activity, not a successful result.' : 'Working — no newer action observed. Inspect the safe action summary and tail.'};
  return {rank:4,rankReason:c.paused?'Intentional lane hold':'Idle lane',label:c.paused?'Idle on purpose':'Idle — reason unavailable',reason:c.paused ? 'Paused by the lane control; no new work will start until it is resumed. Notes and reading remain usable.' : 'Idle — reason not available yet. Idle alone does not establish why work has not started. Review Queue if you expected work.'};
}
function agentOrder(d) {
  const byName = (a,b) => a.lane < b.lane ? -1 : a.lane > b.lane ? 1 : 0;
  const wanted = [...d.lanes].sort((a,b) => (el('agentOrder').value === 'lane' ? 0 : laneExplanation(a).rank-laneExplanation(b).rank) || byName(a,b));
  const existing = Array.from(el('lanes').children).map(n => d.lanes.find(s => cards.get(s.lane)?.root === n)).filter(Boolean);
  const moved = existing.some((s,i) => s.lane !== wanted[i]?.lane);
  const pinned = laneSelection || Array.from(cards.values()).some(v => busy(v.root) || v.tail.open || v.evidence.open) || laneSaves.size;
  el('agentOrderNotice').hidden = !moved || !pinned;
  el('applyAgentOrder').disabled = !!pinned;
  return moved && pinned ? [...existing,...wanted.filter(s => !existing.includes(s))] : wanted;
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
    const indicator = node('span', 'lane-indicator'), label = node('span','lane-tab-reason');
    indicator.setAttribute('aria-hidden', 'true');
    if (s.lane) root.append(indicator,label);
    return {root, indicator, label};
  }, (v,s) => {
    const selected = s.lane === laneSelection;
    v.root.setAttribute('aria-selected', String(selected)); v.root.tabIndex = selected ? 0 : -1;
    v.root.className = 'agent-tab';
    const mark = laneIndicator(s);
    setText(v.label, controlFor(s.lane)?.paused ? ' · Paused' : ' · ' + (mark.kind === 'stuck' ? 'Quiet' : ['attention','failed'].includes(mark.kind) ? 'Review' : mark.label));
    v.indicator.className = 'lane-indicator indicator-' + mark.kind;
    v.root.title = s.lane ? mark.label + ' · ' + (s.display_state || s.state) : 'All agents';
    v.root.setAttribute('aria-label', s.lane ? s.lane + ': ' + mark.label : 'All agents');
  }, s => s.lane);
}
function renderLane(d) {
  el('laneDetail').hidden = !laneSelection;
  if (!laneSelection) return;
  const lane = d.lanes.find(s => s.lane === laneSelection);
  laneTailView.tailState.following = el('followTail').checked && laneTailView.tailState.following;
  updateCardTail(laneTailView, lane || {}, (d.tails || {})[laneSelection] || []);
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
  acceptLaneControls(d.lane_control, d.lane_control_version);
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
  const controls = d.lane_control_version === 1 && validLaneControls(d.lane_control) ? d.lane_control.lanes : null;
  const running = d.lanes.filter(s => s.state === 'running').length;
  const available = controls ? d.lanes.filter(s => {
    const c = controls.find(c => c.name === s.lane);
    return s.state !== 'running' && c?.configured && c.phase === 'enabled' && !c.paused && c.dispatch_eligible && !c.blockers.length && !d.freeze;
  }).length : null;
  const paused = controls ? controls.filter(c => c.paused).length : null;
  const overlap = controls ? d.lanes.filter(s => s.state === 'running' && controls.find(c => c.name === s.lane)?.paused).length : null;
  setText(el('runningCount'), running);
  setText(el('idleCount'), available ?? '—');
  setText(el('pausedCount'), paused ?? '—');
  setText(el('endedCount'), d.lanes.filter(s => ['ended','ended_no_report','failed','cancelled','timed_out','interrupted'].includes(s.state)).length);
  setText(el('countMeaning'), 'One received operational snapshot. Running counts current runs. Available means configured, not busy and no known or unknown holds. Paused includes draining and retained holds; counts overlap, never add them as separate lanes. ' +
    (overlap == null ? 'Lane controls are unknown; Available and Paused counts are unavailable.' : overlap + ' running lane' + (overlap === 1 ? '' : 's') + ' also paused: each is the same lane. Ended counts process outcomes independently of holds.'));
  setText(el('agentsTabCount'), d.lanes.filter(s => s.state === 'running').length);
  setText(el('notesTabCount'), d.notes.filter(n => !n.read).length);
  setText(el('questionsTabCount'), d.history.filter(r => {
    if (r.overview) return r.overview.attention;
    const questions = (r.sections?.QUESTIONS || '').trim();
    return questions && !/^(?:none|none recorded|no open questions)[.!]?$/i.test(questions);
  }).length);
  el('questionsTabCount').title = 'Report attention among ' + d.history.length + ' received attempts; repeated resumed questions are counted per attempt. Review state is not independently checked.';
  renderPanels(d);
  updateMessageControls();
  updateHeldControls();
}
function panelReady(id, shown) {
  const panel = el(id);
  return !panel.hidden && (id === shown || !renderedPanels.has(id) || !busy(panel));
}
function renderPanels(d, shown = '') {
  if (!el('agents').hidden) {
    renderAgentTabs(d);
    reconcile(el('lanes'), agentOrder(d), cards, makeCard, updateCard, s => s.lane);
    for (const [lane,v] of cards) v.root.hidden = !d.lanes.some(s => s.lane === lane) || (!!laneSelection && lane !== laneSelection);
    renderLane(d);
    el('agentsEmpty').hidden = d.lanes.length !== 0;
    renderRetainedHolds();
  }
  if (!el('messages').hidden) renderHeldMessages(d);
  if (panelReady('messages', shown)) {
    fillFilter(el('noteLane'), d.lanes.map(s => s.lane), 'Judge (shared)');
    const select = el('lane'), chosen = select.value;
    for (const option of Array.from(select.options)) if (!d.lanes.some(s => s.lane === option.value)) option.remove();
    for (const s of d.lanes) if (!Array.from(select.options).some(o => o.value === s.lane)) {
      const option = node('option', '', s.lane); option.value = s.lane; select.append(option);
    }
    if (d.lanes.some(s => s.lane === chosen)) select.value = chosen;
    updateMessageControls();
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
async function refresh(replace = false) {
  if (!authenticated()) {
    signedOutView();
    return;
  }
  if (acting || document.hidden || !pageActive) return;
  const request = beginDashboardRead('/data', replace);
  if (!request) return;
  refreshing = true;
  try {
    const {response, data:d} = await request.json();
    if (!request.current()) return;
    if (response.status === 403) { signOut(); return; }
    if (!response.ok) throw new Error('unavailable');
    lastUpdate = Date.now();
    render(d);
    setText(el('lastRefresh'), 'Last refresh ' + new Date(lastUpdate).toLocaleTimeString());
    el('lastRefresh').dateTime = new Date(lastUpdate).toISOString();
    el('lastRefresh').title = new Date(lastUpdate).toLocaleString();
    el('updated').title = el('lastRefresh').title;
    el('error').hidden = true;
  } catch {
    if (!request.current()) return;
    setText(el('error'), 'Unable to refresh dashboard. Showing the last received data. Retrying every 2 seconds.');
    el('error').hidden = false;
  } finally { if (request.finish()) refreshing = false; }
}
function signedOutView() {
  clearLaneControls();
  lastData = undefined;
  for (const cache of [cards, runs, notes, laneRuns, audits, agentTabs, heldRows, retainedHolds]) {
    for (const v of cache.values()) {
      v.resetCancel?.(); v.detail?.replaceChildren(); v.tailText?.replaceChildren();
      if (v.tailState) { v.tailState.lines = []; v.tailState.run = ''; }
      v.data = undefined; v.root.replaceChildren(); v.root.remove();
    }
    cache.clear();
  }
  renderedPanels.clear(); laneSelection = '';
  el('agentOrderNotice').hidden = true;
  setText(el('messageHold'),'');
  setText(el('heldSummary'),''); setText(el('countMeaning'),'');
  el('heldMessages').replaceChildren(); el('retainedHolds').replaceChildren(); el('retainedSection').hidden = true;
  el('lane').replaceChildren(); el('lane').value = ''; el('noteLane').replaceChildren(); el('noteLane').value = '';
  for (const id of ['auditLane','auditAction']) { el(id).replaceChildren(); el(id).value = ''; }
  el('auditFilter').value = '';
  setText(el('auditCount'),'');
  for (const id of ['message','note','filter']) el(id).value = '';
  for (const id of ['laneTail','laneNotes','laneSignals','oversight','historyBound']) { el(id).replaceChildren(); delete el(id).dataset.value; }
  el('laneDetail').hidden = true;
  Object.assign(laneTailView.tailState, {lines: [], run: '', following: true, pending: 0});
  laneTailView.tailJump.hidden = true;
  setText(laneTailView.tailAge, ''); setText(laneTailView.tailNotice, '');
  for (const tab of tabs) el(tab.getAttribute('aria-controls')).hidden = true;
  el('signInPanel').hidden = false;
  setText(el('signInText'), 'Open the link from `nuntius web link` or iMessage to sign in.');
  stateBadge(el('freeze'), 'Signed out');
  el('freeze').title = 'Signed out';
  el('freeze').setAttribute('aria-label', 'Signed out');
  for (const id of ['runningCount','idleCount','pausedCount','endedCount','agentsTabCount','notesTabCount','questionsTabCount','queueTabCount']) setText(el(id), '—');
  el('questionsTabCount').title = '';
  lastUpdate = 0;
  setText(el('updated'), 'Waiting for first update');
  setText(el('lastRefresh'), 'Last refresh —');
  el('updated').title = ''; el('lastRefresh').title = '';
  el('lastRefresh').removeAttribute('datetime');
  for (const id of ['freezeButton','unfreezeButton','signOut','overview','error','notice']) el(id).hidden = true;
}
async function action(name, lane = '', text = '', stop = false) {
  if (!authenticated()) { await refresh(); return false; }
  if (name === 'message' && messageGuard(lane)) { updateMessageControls(); return false; }
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
    for (const [name,view] of cards) updateLaneControls(view,name);
    updateMessageControls();
    updateHeldControls();
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
el('agentOrder').addEventListener('change', () => { if (lastData) renderPanels(lastData); });
el('applyAgentOrder').addEventListener('click', () => { if (lastData) renderPanels(lastData); });
el('lane').addEventListener('change',updateMessageControls);
el('auditFilter').addEventListener('input', filterAudit);
el('auditLane').addEventListener('change', filterAudit);
el('auditAction').addEventListener('change', filterAudit);
el('auditReads').addEventListener('change', () => {
  try { localStorage.setItem(auditReadsKey, String(el('auditReads').checked)); } catch {}
  filterAudit();
});
document.addEventListener('visibilitychange', foregroundRefresh);
window.addEventListener('focus', foregroundRefresh);
window.addEventListener('pageshow', foregroundRefresh);
window.addEventListener('pagehide', () => { pageActive = false; cancelDashboardReads(); settingsUpdate(); });
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
  machine.append(options,node('p','hint','Queue file · '+(c.queue_file_mode||'default')+' · local only'),node('p','hint',c.local_only_reason));
}
function settingsFailure(code) {
  return {locked:'Settings busy, retrying. Your draft is kept.',configuration_unavailable:'Settings configuration source is unavailable. Run nuntius config check locally, then reload current values. Your draft is kept.',invalid_configuration:'Settings configuration is invalid or unreadable. Run nuntius config check locally, then reload current values. Your draft is kept.',rate_limited:'Too many settings requests. Wait briefly, then reload current values. Your draft is kept.'}[code] || 'Settings unavailable. Your draft is kept. Reload current values before saving.';
}
function settingsReadFailure(text) {
  settingsBanner(text + (settingsFetched ? ' Last updated ' + Math.floor((Date.now()-settingsFetched)/1000) + 's ago.' : ''), true);
}
async function loadSettings(force = false, retry = 0) {
  if (!authenticated() || document.hidden || !pageActive || settingsSaving || (!force && settingsReadAt && Date.now() - settingsReadAt < 10000)) return;
  const request = beginDashboardRead('/settings', force);
  if (!request) return;
  clearTimeout(settingsRetry); settingsRetry = null;
  settingsLoading = true; settingsReadAt = Date.now(); const epoch = authEpoch;
  settingsUpdate();
  try {
    const {response, data:c} = await request.json();
    if (!request.current()) return;
    if (response.status === 403) { signOut(); return; }
    if (!response.ok) {
      settingsConfirmed = false;
      const again = c.code === 'locked' && retry < 3;
      settingsReadFailure(c.code === 'locked' && !again ? 'Settings busy. Reload current values to retry. Your draft is kept.' : settingsFailure(c.code));
      if (again) settingsRetry = setTimeout(() => {
        if (epoch !== authEpoch || !authenticated()) return;
        settingsRetry = null;
        if (el('settings').hidden || document.hidden) { settingsReadAt = 0; return; }
        loadSettings(true, retry + 1);
      }, 1000);
      return;
    }
    const dirty = settingsDiff().length > 0;
    settingsCurrent = c; settingsConfirmed = true; settingsFetched = Date.now(); settingsReadOnlyView(c);
    if (!dirty) settingsResetDraft();
    settingsBanner(dirty ? 'Current values refreshed. Your draft is kept; review it before saving.' : '');
  } catch {
    if (!request.current()) return;
    settingsConfirmed = false;
    settingsReadFailure(settingsFailure('settings_unavailable'));
  } finally { if (request.finish()) { settingsLoading=false; settingsUpdate(); } }
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
      const messages = {locked:'Nothing saved. Settings are busy. Reload current values and retry your draft.',configuration_unavailable:settingsFailure('configuration_unavailable'),invalid_configuration:settingsFailure('invalid_configuration'),busy_agent:'Nothing saved. A busy agent cannot be removed. Keep the pool large enough for running agents.',revision_conflict:'Nothing saved. Settings changed elsewhere. Reload current values and review your draft.',recipient_not_configured:'Configure an iMessage recipient locally before enabling notifications.',rate_limited:'Too many settings requests. Wait briefly, then reload current values.',committed_audit_failed:'Settings were committed, but audit completion failed. Check current values before trying again.'};
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
  clearTimeout(settingsRetry); settingsRetry = null;
  settingsCurrent=null; settingsDraft=null; settingsConfirmed=false; settingsLoading=false; settingsSaving=false; settingsReadAt=0; settingsFetched=0;
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
el('settingsReload').addEventListener('click', () => loadSettings(true));
setInterval(() => {
  if (lastUpdate) setText(el('updated'), 'Updated ' + Math.floor((Date.now() - lastUpdate) / 1000) + 's ago');
  if (lastData) {
    for (const v of cards.values()) { updateTailAge(v); updateCardAge(v); }
    updateTailAge(laneTailView);
    for (const cache of [runs, laneRuns]) for (const v of cache.values()) if (!v.detail.hidden) updateRunAge(v, v.data);
  }
}, 1000);
// The selected-lane surface shares the card consumer and its poll/stream entry.
{
  const tailText = el('laneTail'), tailAge = node('p', 'tail-age'), tailNotice = node('p', 'muted');
  const binding = bindTail(tailText);
  const tail = {open: true};
  laneTailView = {tailText, tailAge, tailNotice, tail, ...binding};
  for (const n of [tailAge, binding.tailJump, tailNotice]) tailText.parentElement.insertBefore(n, tailText);
  binding.tailJump.addEventListener('click', () => {
    el('followTail').checked = true; binding.tailState.following = true;
    binding.tailState.pending = 0; binding.tailJump.hidden = true; tailText.scrollTop = tailText.scrollHeight;
  });
  el('followTail').addEventListener('change', () => {
    binding.tailState.following = el('followTail').checked;
    if (binding.tailState.following) { binding.tailState.pending = 0; binding.tailJump.hidden = true; tailText.scrollTop = tailText.scrollHeight; }
  });
}
if (!token) clearQueue();
selectTab(location.hash.slice(1), false, false);
refresh();
setInterval(refresh, 2000);
setInterval(() => { loadQueue(); if (!el('settings').hidden) loadSettings(); }, 10000);

function clearQueue() {
  queueData=null; queueFetched=0; queueFailure=''; queueRows.clear();
  el('queueRows').replaceChildren(); setText(el('queueTabCount'),'—');
  setText(el('queueSnapshot'),''); setText(el('queueBanner'),''); el('queueBanner').hidden=true;
  el('queueSearch').value=''; el('queueKind').value=''; el('queueState').value=''; el('queueOwner').checked=false;
  for(const id of ['queueFilters','queueResults','queueSnapshot'])el(id).hidden=true;
  queueEmpty('Sign in to view the queue','Open your private Nuntius sign-in link.');
}
function queueEmpty(title,text) {
  el('queueEmpty').hidden=false; el('queueTable').hidden=true;
  setText(el('queueEmptyTitle'),title);setText(el('queueEmptyText'),text);
}
function queueSafeLink(value) {
  try {const u=new URL(value), h=u.hostname.toLowerCase().replace(/\.$/,'');
    return u.protocol==='https:' && !u.username && !u.password && !u.search && !u.hash && !/[?#]/.test(value) &&
      h.includes('.') && !/^[\d.]+$/.test(h) && !h.includes(':') && h!=='localhost' && !h.endsWith('.localhost') && !h.endsWith('.local');
  } catch {return false;}
}
function makeQueueRow() {
  const root=node('tr'), order=node('td','queue-order'), goal=node('td','queue-goal'), title=node('p','queue-title'), id=node('p','queue-id');
  const detail=node('details'), dates=node('p','muted'), reason=node('p','muted'), digest=node('p','queue-id'), link=node('a','','Read brief');
  link.rel='noreferrer noopener';link.target='_blank';detail.append(node('summary','','Details'),dates,reason,digest,link);goal.append(title,id,detail);
  const lane=node('td','queue-lane'), state=node('td','queue-state'), badge=node('span','badge');state.append(badge);
  const wait=node('td','queue-wait'), owner=node('div','queue-owner'), ownerReason=node('p'), deps=node('ul','queue-deps'), fallback=node('p','muted');
  owner.append(node('span','badge amber','Needs owner'),ownerReason);wait.append(node('span','queue-mobile-label','Waiting on'),owner,deps,fallback);
  root.append(order,goal,lane,state,wait);return {root,order,title,id,detail,dates,reason,digest,link,lane,badge,owner,ownerReason,deps,fallback};
}
function renderQueue() {
  el('queueBanner').hidden=true;
  const ok=queueData?.status==='ok';
  el('queueFilters').hidden=!ok;el('queueResults').hidden=!ok;el('queueSnapshot').hidden=!ok;
  setText(el('queueTabCount'),ok?queueData.goals.length:'—');
  if (!ok) {
    const copy={not_configured:['No queue has been published','Your coordinator can write queue.json in the Nuntius data folder. Queue only displays planned goals.'],disabled:['Queue is disabled','Queue is disabled in local configuration.'],missing:['Queue file is unavailable','Ask the local owner or coordinator to check the configured file.'],invalid:['Queue data could not be read','The coordinator must publish a valid snapshot.'],unavailable:['Queue file is unavailable','Ask the local owner or coordinator to check the configured file.']};
    queueEmpty(...(copy[queueFailure || queueData?.status] || ['Loading queue','Waiting for the coordinator snapshot.']));return;
  }
  setText(el('queueSnapshot'),'Coordinator snapshot '+queueData.updated+' · Last successful fetch '+new Date(queueFetched).toISOString());
  const old=Date.now()-Date.parse(queueData.updated)>15*60*1000;
  if (queueFailure || old) {el('queueBanner').hidden=false;setText(el('queueBanner'),queueFailure?'Last known queue · refresh unavailable. Last updated '+Math.floor((Date.now()-queueFetched)/1000)+'s ago. These goals may have changed.':'Coordinator snapshot is old; check with the coordinator.');}
  fillFilter(el('queueKind'),[...queueData.goals.flatMap(g=>g.lane_kinds),el('queueKind').value],'All kinds');
  const search=el('queueSearch').value.trim().toLowerCase(),kind=el('queueKind').value,state=el('queueState').value,owner=el('queueOwner').checked;
  const filtered=!!(search||kind||state||owner),items=queueData.goals.filter(g=>(!search||(g.id+' '+g.title).toLowerCase().includes(search))&&(!kind||g.lane_kinds.includes(kind))&&(!state||g.state===state)&&(!owner||g.needs_owner)).sort((a,b)=>a.order-b.order||(a.id<b.id?-1:a.id>b.id?1:0));
  setText(el('queueCount'),items.length+' of '+queueData.goals.length+' planned goals');el('queueReset').hidden=!filtered;
  const known=new Set(queueData.goals.map(g=>g.id));
  const ordered=[...queueData.goals].sort((a,b)=>a.order-b.order||(a.id<b.id?-1:a.id>b.id?1:0));
  reconcile(el('queueRows'),ordered,queueRows,makeQueueRow,(v,g)=>{
    v.root.hidden=!items.includes(g);if(busy(v.root))return;
    v.root.className=g.needs_owner?'queue-needs-owner':'';
    setText(v.order,g.order);setText(v.title,g.title);setText(v.id,g.id);
    setText(v.dates,'Created '+g.created+' · Updated '+g.updated);setText(v.reason,g.reason||'');setText(v.digest,g.brief_sha256?'Brief SHA-256 '+g.brief_sha256:'');
    v.link.hidden=!g.brief_link||!queueSafeLink(g.brief_link);if(!v.link.hidden)v.link.href=g.brief_link;else v.link.removeAttribute?.('href');
    setText(v.lane,g.size+' · '+g.lane_kinds.join(' / ')+(g.lane?' · Lane: '+g.lane:''));
    stateBadge(v.badge,g.state,g.state==='ready'?'blue':'');v.owner.hidden=!g.needs_owner;setText(v.ownerReason,g.owner_reason||'');
    const depText=g.depends_on.map(d=>d+(known.has(d)?'':' · outside this queue'));
    if(v.deps.textContent!==depText.join(''))v.deps.replaceChildren(...depText.map(d=>node('li','',d)));
    setText(v.fallback,!g.depends_on.length&&!g.needs_owner?(g.state==='ready'?'No recorded holds':g.reason||'Being prepared'):'');
  },g=>g.id);
  for(const [id,view] of queueRows)if(!known.has(id)){view.root.remove();queueRows.delete(id);}
  el('queueEmpty').hidden=true;el('queueTable').hidden=false;
  if(!items.length)queueEmpty(queueData.goals.length?'No matching goals':'No planned goals',queueData.goals.length?'Try another lane kind or state, or clear your filters.':'Running and finished work appears in Agents and History.');
}
async function loadQueue(replace = false) {
  if (!authenticated() || document.hidden || !pageActive || el('queue').hidden) return;
  const request = beginDashboardRead('/queue', replace);
  if (!request) return;
  queueLoading=true;
  try {
    const {response, data} = await request.json();
    if(!request.current())return;
    if(response.status===403){signOut();return;}
    if(response.ok&&(data.status==='ok'||data.status==='disabled'||data.status==='not_configured')){
      queueData=data;queueFailure='';queueFetched=Date.now();
      if(data.status!=='ok'){queueRows.clear();el('queueRows').replaceChildren();}
    } else queueFailure=['missing','invalid','unavailable'].includes(data.status)?data.status:'unavailable';
    renderQueue();
  } catch {if(request.current()){queueFailure='unavailable';renderQueue();}}
  finally {if(request.finish())queueLoading=false;}
}
for(const id of ['queueSearch','queueKind','queueState','queueOwner'])el(id).addEventListener(id==='queueSearch'?'input':'change',renderQueue);
el('queueReset').addEventListener('click',()=>{el('queueSearch').value='';el('queueKind').value='';el('queueState').value='';el('queueOwner').checked=false;renderQueue();});
