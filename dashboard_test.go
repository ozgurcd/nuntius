package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDashboardAssetsBootstrap(t *testing.T) {
	m, _, _ := manager(t)
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range []struct{ path, contentType string }{
		{"/", "text/html; charset=utf-8"},
		{"/app.css", "text/css; charset=utf-8"},
		{"/app.js", "text/javascript; charset=utf-8"},
	} {
		for _, auth := range []bool{false, true} {
			path := asset.path
			if auth {
				path += "?token=" + w.token
			}
			req := httptest.NewRequest("GET", "http://localhost:8123"+path, nil)
			out := httptest.NewRecorder()
			w.ServeHTTP(out, req)
			if out.Code != 200 || out.Header().Get("Content-Type") != asset.contentType || out.Body.Len() == 0 {
				t.Fatal("asset unavailable")
			}
			for key, want := range map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff"} {
				if out.Header().Get(key) != want {
					t.Fatal("asset header missing")
				}
			}
			embedded := "index.html"
			if asset.path != "/" {
				embedded = strings.TrimPrefix(asset.path, "/")
			}
			static, err := dashboardAssets.ReadFile(embedded)
			if err != nil || !bytes.Equal(out.Body.Bytes(), static) || strings.Contains(out.Body.String(), w.token) {
				t.Fatal("shell must be static and contain no credential or private data")
			}
			if asset.path == "/" {
				if !strings.Contains(out.Body.String(), `src="/app.js"`) || !strings.Contains(out.Body.String(), `href="/app.css"`) || strings.Contains(out.Body.String(), "<style") || strings.Contains(out.Body.String(), "<script>") {
					t.Fatal("external asset bootstrap missing")
				}
			}
		}
	}
	page, err := dashboardAssets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	if strings.Count(html, `role="tablist"`) != 2 || strings.Count(html, `role="tab"`) != 5 || strings.Count(html, `role="tabpanel"`) != 5 || strings.Count(html, `aria-selected="true"`) != 1 {
		t.Fatal("dashboard needs five accessible tabs and one initial selection")
	}
	for _, id := range []string{"agents", "messages", "digest", "runs", "auditSection"} {
		if !strings.Contains(html, `id="tab-`+id+`"`) || !strings.Contains(html, `aria-controls="`+id+`"`) || !strings.Contains(html, `aria-labelledby="tab-`+id+`"`) {
			t.Fatal("tab and panel association missing")
		}
	}
	if !strings.Contains(html, `id="tab-agents" role="tab" aria-selected="true" tabindex="0"`) || strings.Count(html, `role="tabpanel" hidden`) != 4 {
		t.Fatal("Agents must be the only initially visible panel")
	}
	script, err := dashboardAssets.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, contract := range []string{"selectTab(location.hash.slice(1), false, false)", "'/' + location.hash", "'hashchange'", "'ArrowLeft'", "'ArrowRight'", "'Home'", "'End'", "panel.hidden = !selected", "tab.setAttribute('aria-selected', String(selected))", "tab.tabIndex = selected ? 0 : -1"} {
		if !strings.Contains(string(script), contract) {
			t.Fatalf("tab source contract missing: %s", contract)
		}
	}
}

func TestDashboardRuntime(t *testing.T) {
	page, err := dashboardAssets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	script, err := dashboardAssets.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	// Node evaluates the shipped script offline, without a browser or network.
	fixture, err := os.ReadFile("testdata/dashboard-data.json")
	if err != nil {
		t.Fatal(err)
	}
	css, err := dashboardAssets.ReadFile("app.css")
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{"data": json.RawMessage(fixture), "script": string(script), "page": string(page), "css": string(css)})
	if err != nil {
		t.Fatal("cannot encode dashboard test input")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "-e", dashboardRuntimeTest)
	cmd.Stdin = bytes.NewReader(input)
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err != nil {
		// The harness prints only named predicates, never test inputs or auth values.
		t.Fatalf("offline dashboard runtime predicates failed: %v (%s)", err, diagnostic.String())
	}
}

const dashboardRuntimeTest = `
const vm = require('node:vm');
const input = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
let predicate = 'harness initialization';
function check(ok, label) { predicate = label; if (!ok) throw new Error(); }
class Element {
  constructor(tag) { this.tagName = tag.toUpperCase(); this.children = []; this.attrs = {}; this.value = ''; this.hidden = false; this.disabled = false; this.open = false; this.checked = false; this.dataset = {}; this.listeners = {}; this.className = ''; this.text = ''; }
  get textContent() { return this.text + this.children.map(n => n.textContent).join(''); }
  set textContent(value) { this.replaceChildren(); this.text = String(value); }
  get firstElementChild() { return this.children[0] || null; }
  get nextElementSibling() { const siblings = this.parentElement.children; return siblings[siblings.indexOf(this) + 1] || null; }
  get options() { return this.children; }
  get classList() { return {add: c => { this.className += ' ' + c; }, remove: c => { this.className = this.className.split(' ').filter(x => x !== c).join(' '); }, contains: c => this.className.split(' ').includes(c)}; }
  setAttribute(key, value) { this.attrs[key] = String(value); if (key === 'id') this.id = value; if (key === 'class') this.className = value; if (key === 'hidden') this.hidden = true; if (key === 'checked') this.checked = true; }
  getAttribute(key) { return this.attrs[key]; }
  contains(n) { return n === this || this.children.some(child => child.contains(n)); }
  matches(s) { return s.startsWith('.') ? this.classList.contains(s.slice(1)) : s === '[role="tab"]' ? this.attrs.role === 'tab' : this.tagName === s.toUpperCase(); }
  closest(s) { return s.split(', ').some(x => this.matches(x)) ? this : this.parentElement?.closest(s); }
  querySelectorAll(s) { return this.children.flatMap(n => [...(n.matches(s) ? [n] : []), ...n.querySelectorAll(s)]); }
  querySelector(s) { return this.querySelectorAll(s)[0] || null; }
  append(...nodes) { nodes.forEach(n => this.insertBefore(n, null)); }
  insertBefore(n, cursor) { n.remove(); const i = cursor ? this.children.indexOf(cursor) : this.children.length; this.children.splice(i, 0, n); n.parentElement = this; }
  remove() { if (this.parentElement) { const a = this.parentElement.children; a.splice(a.indexOf(this), 1); this.parentElement = null; } }
  replaceChildren(...nodes) { [...this.children].forEach(n => n.remove()); this.text = ''; this.append(...nodes); }
  addEventListener(name, fn) { this.listeners[name] = fn; }
  click() { if (!this.disabled) return this.listeners.click?.(); }
  focus() { this.document.activeElement = this; }
}
function start(hash, store = new Map(), query = '', blocked = false, clock = {now: 1791658800000}, status = 200, pending = null, pendingAction = null) {
  const root = new Element('html'), ids = {}, stack = [root];
  // Parse the real static shell's element tree; no HTML renderer is involved.
  for (const match of input.page.matchAll(/<\/?([a-z][a-z0-9]*)([^>]*)>/gi)) {
    if (match[0].startsWith('</')) { if (stack.length > 1) stack.pop(); continue; }
    const n = new Element(match[1]);
    for (const a of match[2].matchAll(/([\w-]+)(?:="([^"]*)")?/g)) n.setAttribute(a[1], a[2] || '');
    if (n.id) ids[n.id] = n;
    stack.at(-1).append(n);
    if (!['meta', 'link', 'input'].includes(match[1])) stack.push(n);
  }
  const doc = {hidden: false, documentElement: root, activeElement: ids[hash.slice(1)] || root, currentScript: {src: 'http://localhost:8123/app.js' + query}, getElementById: id => ids[id], querySelectorAll: s => root.querySelectorAll(s), createElement: tag => { const n = new Element(tag); n.document = doc; return n; }, addEventListener: (n, fn) => { doc[n] = fn; }};
  function attach(n) { n.document = doc; n.children.forEach(attach); } attach(root);
  const calls = [], timers = [], win = {getSelection: () => ({isCollapsed: true}), addEventListener: (n, fn) => { win[n] = fn; }};
  const location = {href: 'http://localhost:8123/' + query + hash, hash, search: query, reloads: 0, reload: () => { location.reloads++; }};
  const storage = {getItem: k => { if (blocked) throw new Error(); return store.get(k) || null; }, setItem: (k, v) => { if (blocked) throw new Error(); store.set(k, v); }, removeItem: k => { if (blocked) throw new Error(); store.delete(k); }};
  // Each start is a fresh tab: only origin localStorage survives discard.
  const session = new Map();
  class ClockDate extends Date { static now() { return clock.now; } }
  const context = vm.createContext({Date: ClockDate, document: doc, window: win, location, URL, history: {pushState: (_, __, path) => { location.hash = path.slice(path.indexOf('#')); }, replaceState: (_, __, path) => { location.href = 'http://localhost:8123' + path; location.hash = path.includes('#') ? path.slice(path.indexOf('#')) : ''; location.search = ''; }}, localStorage: storage, sessionStorage: {getItem: k => session.get(k) || null, setItem: (k,v) => session.set(k,v), removeItem: k => session.delete(k)}, fetch: async (path, options) => { calls.push({path, options}); if (path === '/action' && pendingAction) return await pendingAction; if (pending) await pending; return {ok: status === 200, status, json: async () => input.data}; }, setInterval: fn => { timers.push(fn); }, setTimeout: () => 1, clearTimeout: () => {}});
  vm.runInContext(input.script, context);
  return {context, ids, calls, timers, doc, win, location};
}
async function settle() { for (let i = 0; i < 8; i++) await Promise.resolve(); }
function rendered(h, id) {
  if (id === 'runs') return h.ids.runs.querySelectorAll('tbody').length === input.data.history.length;
  if (id === 'messages') return h.ids.notes.children.length === input.data.notes.length && h.ids.lane.options.length === input.data.lanes.length;
  if (id === 'digest') return h.ids.oversight.children.length > 0;
  return h.ids.auditRows.children.length === input.data.audit.length;
}
(async () => {
  const phoneFailures = [];
  function phoneCheck(ok, label) { if (!ok) phoneFailures.push(label); }
  const clock = {now: 1791658800000}, sevenDays = 7 * 24 * 60 * 60 * 1000;
  const device = new Map();
  const signedIn = start('#agents', device, '?token=fixture-auth', false, clock); await settle();
  let saved;
  try { saved = JSON.parse(device.get('nuntius.web.credential')); } catch {}
  phoneCheck(saved?.token === 'fixture-auth' && saved?.expiresAt === clock.now + sevenDays, 'phone saves an absolute bounded device grant');
  phoneCheck(!device.has('nuntius.web.token'), 'legacy tab key is not persisted');
  phoneCheck(!signedIn.location.search && !signedIn.location.href.includes('fixture-auth'), 'phone bootstrap removes grant from URL');
  const discarded = start('#runs', device, '', false, clock); await settle();
  phoneCheck(discarded.calls.length === 1 && rendered(discarded, 'runs'), 'bookmark survives discarded tab with fresh sessionStorage');
  phoneCheck(device.get('nuntius.web.credential') === (saved && JSON.stringify(saved)), 'bookmark does not renew grant lifetime');
  clock.now += sevenDays;
  const expired = start('#agents', device, '', false, clock); await settle();
  phoneCheck(expired.calls.length === 0 && !device.has('nuntius.web.credential') && !expired.ids.error.hidden, 'exact expiry clears stored grant and requests sign-in');
  const invalidRecords = ['broken', '{}', 'null', JSON.stringify({token:'fixture-auth',expiresAt:clock.now + sevenDays + 1}), JSON.stringify({token:'fixture-auth',expiresAt:'later'})];
  for (const record of invalidRecords) {
    const store = new Map([['nuntius.web.credential',record]]);
    const h = start('#agents', store, '', false, clock); await settle();
    phoneCheck(h.calls.length === 0 && !store.has('nuntius.web.credential'), 'invalid or overlong stored grant fails closed');
  }
  const signoutStore = new Map();
  const signout = start('#agents', signoutStore, '?token=fixture-auth', false, clock); await settle();
  phoneCheck(signout.ids.signOut?.listeners.click !== undefined, 'explicit phone sign-out control exists');
  signout.ids.signOut?.click();
  await vm.runInContext("action('unfreeze'); refresh()", signout.context); await settle();
  phoneCheck(!signoutStore.has('nuntius.web.credential') && signout.location.reloads === 1 && signout.calls.length === 1, 'sign-out clears grant and navigates away from private data without further requests');
  const secondTab = start('#agents', new Map(), '?token=fixture-auth', false, clock); await settle();
  if (secondTab.win.storage) secondTab.win.storage({key:'nuntius.web.credential', newValue:null, storageArea:vm.runInContext('localStorage',secondTab.context)});
  await vm.runInContext("action('unfreeze'); refresh()", secondTab.context); await settle();
  phoneCheck(secondTab.location.reloads === 1 && secondTab.calls.length === 1, 'storage event signs out other open tabs');
  const revokedStore = new Map([['nuntius.web.credential',JSON.stringify({token:'fixture-auth',expiresAt:clock.now+sevenDays})]]);
  const revoked = start('#runs', revokedStore, '', false, clock, 403); await settle();
  phoneCheck(!revokedStore.has('nuntius.web.credential') && revoked.location.reloads === 1, 'server revocation removes remembered grant');
  const transientStore = new Map([['nuntius.web.credential',JSON.stringify({token:'fixture-auth',expiresAt:clock.now+sevenDays})]]);
  const transient = start('#runs', transientStore, '', false, clock, 503); await settle();
  phoneCheck(transientStore.has('nuntius.web.credential') && transient.location.reloads === 0, 'temporary server failure retains remembered sign-in');
  const activeClock = {now: clock.now}, activeStore = new Map();
  const active = start('#agents',activeStore,'?token=fixture-auth',false,activeClock); await settle();
  activeClock.now += sevenDays;
  await vm.runInContext("action('unfreeze'); refresh()",active.context); await settle();
  phoneCheck(active.calls.length === 1 && !activeStore.has('nuntius.web.credential') && active.location.reloads === 1, 'open page expiry blocks actions and polling');
  let release;
  const deferred = new Promise(resolve => { release = resolve; });
  const inflight = start('#runs',new Map(),'?token=fixture-auth',false,clock,200,deferred);
  inflight.ids.signOut?.click();
  release(); await settle();
  phoneCheck(!rendered(inflight,'runs'), 'sign-out ignores a late authenticated response');
  if (phoneFailures.length) { predicate = phoneFailures.join('; '); throw new Error(); }
  // User clicks respect disabled controls; action completion is held separately
  // from the initial refresh, using only offline responses and fresh VM tabs.
  for (const result of [200, 403, 503, 'reject']) {
    let finish, reject;
    const response = new Promise((resolve, fail) => { finish = resolve; reject = fail; });
    const store = new Map();
    const h = start('#runs', store, '?token=fixture-auth', false, clock, 200, null, response); await settle();
    const before = h.ids.runs.textContent;
    const pendingAction = h.ids.freezeButton.click();
    check(h.calls.length === 2 && h.calls[1].path === '/action' && h.ids.freezeButton.disabled, 'action is pending and locks action controls');
    h.ids.freezeButton.click();
    check(h.calls.length === 2, 'disabled-aware click suppresses a locked action');
    check(!h.ids.signOut.disabled, 'Sign out stays enabled during a pending action');
    h.ids.signOut.click();
    check(!store.has('nuntius.web.credential') && h.location.reloads === 1 && !vm.runInContext('authenticated()', h.context), 'pending-action Sign out clears stored and in-memory credentials immediately and reloads the shell');
    const shell = start('#runs', store, '', false, clock); await settle();
    check(shell.calls.length === 0 && !rendered(shell, 'runs') && shell.ids.signOut.hidden, 'signed-out shell has no private data or authenticated requests');
    if (result === 'reject') reject(new Error());
    else finish({ok: result === 200, status: result, json: async () => input.data});
    check(await pendingAction === false, 'late signed-out action never reports success: ' + result);
    await settle();
    check(!store.has('nuntius.web.credential') && !vm.runInContext('authenticated()', h.context) && h.location.reloads === 1, 'late action cannot restore credentials or repeat sign-out: ' + result);
    check(h.ids.notice.hidden && h.ids.error.hidden && h.ids.runs.textContent === before && h.calls.length === 2, 'late action cannot restore notices, private data or refresh requests: ' + result);
  }
	const phoneWidth = 390;
	const phoneRules = [...input.css.matchAll(/@media\s*\(max-width:\s*(\d+)px\)\s*\{([^@]*?)(?=@media|$)/g)].filter(r => phoneWidth <= Number(r[1])).map(r => r[2]).join('\n');
	check(/\.table-scroll\s*\{[^}]*overflow-x:\s*auto/.test(phoneRules) && /\.table-scroll td\s*\{[^}]*white-space:\s*nowrap/.test(phoneRules), '390px wide-table cells stay intact inside a scrolling wrapper');
	check(/\.live-tail\s*\{[^}]*max-width:\s*100%[^}]*white-space:\s*pre-wrap[^}]*overflow-wrap:\s*anywhere/.test(input.css), 'long live-tail paths wrap inside the phone panel');
  for (const id of ['runs', 'messages', 'digest', 'auditSection']) {
    predicate = 'hash-load ' + id;
    const h = start('#' + id, new Map(), '?token=fixture-auth');
    await settle();
    check(rendered(h, id), 'focused hash panel renders: ' + id);
    check(h.calls.length === 1 && h.calls[0].path === '/data', 'authenticated initial data fetch');
    check(!h.location.search, 'bootstrap removes URL query');
    const store = new Map();
    const first = start('#agents', store, '?token=fixture-auth'); await settle();
    const reloaded = start('#' + id, store); await settle();
    check(rendered(reloaded, id), 'reload retains hash and authentication: ' + id);
    check(reloaded.calls.length === 1 && reloaded.calls[0].options.headers['X-Nuntius-Token'] === first.calls[0].options.headers['X-Nuntius-Token'], 'reload uses device storage');
    // A hidden panel can retain focus and a recent hold from a prior interaction.
    vm.runInContext("holds.set(el('" + id + "'), Date.now() + 5000)", h.context);
    vm.runInContext("selectTab('agents')", h.context);
    vm.runInContext("lastData = JSON.parse(JSON.stringify(lastData)); lastData.notes[0].text = 'Updated owner note'; lastData.oversight = 'Updated digest'; lastData.audit = [{time: '2026-10-10T00:00:00Z', action: 'freeze', summary: 'Updated audit', result: 'ok', lane: '', goal_id: '', reason: '', digest: '', raw: '{}'}]; lastData.history = lastData.history.slice(0, 1); render(lastData)", h.context);
    if (id === 'runs') h.ids.filter.value = input.data.history[0].goal_id;
    vm.runInContext("selectTab('" + id + "')", h.context);
    const updated = id === 'runs' ? h.ids.runs.querySelectorAll('tbody').length === 1 : id === 'messages' ? h.ids.notes.textContent.includes('Updated owner note') : id === 'digest' ? h.ids.oversight.textContent.includes('Updated digest') : h.ids.auditRows.textContent.includes('Updated audit');
    check(updated, 'busy panel receives changed data when made visible: ' + id);
    if (id === 'runs') check(h.ids.runs.querySelectorAll('tbody').filter(n => !n.hidden).length === 1, 'history applies deferred filter on visibility');
    check(vm.runInContext("busy(el('" + id + "'))", h.context), 'recent interaction still pauses subsequent refresh');
    vm.runInContext("holds.delete(el('" + id + "'))", h.context);
    check(!vm.runInContext("busy(el('" + id + "'))", h.context), 'panel element focus alone is not interaction');
    h.ids.filter.focus();
    check(vm.runInContext("busy(el('runs'))", h.context), 'input focus remains protected');
  }
  const lane = start('#agents/codex-2', new Map(), '?token=fixture-auth'); await settle();
  check(lane.location.hash === '#agents/codex-2', 'lane hash retained on load');
  check(lane.ids.agentTabs.children.length === input.data.lanes.length + 1, 'All and per-lane tabs');
  check(lane.ids.lanes.children.filter(n => !n.hidden).length === 1 && lane.ids.lanes.textContent.includes('codex-2'), 'selected lane card only');
  check(lane.ids.laneHistory.textContent.includes('REVIEW-RELEASE') && !lane.ids.laneHistory.textContent.includes('CHECK-DOCS'), 'lane history isolates runs');
  check(lane.ids.laneTail.textContent.startsWith('Report saved.'), 'lane tail newest first');
  check(lane.ids.laneNotes.textContent.includes('codex-2 note') && !lane.ids.laneNotes.textContent.includes('claude-1 note'), 'lane owner notes isolated');
  check(lane.ids.laneSignals.textContent.includes('codex-2.REVIEW-RELEASE.ended'), 'lane signals visible');
  vm.runInContext("lastData = JSON.parse(JSON.stringify(lastData)); lastData.lanes.push({lane:'codex-3',kind:'codex',model:'runtime',effort:'default',state:'idle',eta:'-',activity:'-'}); render(lastData)",lane.context);
  check(lane.ids.agentTabs.children.length === input.data.lanes.length + 2, 'runtime lane added');
  vm.runInContext("selectTab('agents/codex-3')",lane.context);
  check(lane.location.hash === '#agents/codex-3' && lane.ids.lanes.textContent.includes('runtime'), 'runtime lane routing and model');
  lane.location.hash = '#agents/codex-2'; lane.win.hashchange();
  check(lane.ids.laneHistory.textContent.includes('REVIEW-RELEASE'), 'back/forward hash routing');
  vm.runInContext("lastData.tails['codex-2'].push('Newest action'); render(lastData)",lane.context);
  check(lane.ids.laneTail.textContent.startsWith('Newest action'), 'live lane tail follows');
  vm.runInContext("lastData.lanes = lastData.lanes.filter(s => s.lane !== 'codex-2'); render(lastData)",lane.context);
  check(lane.location.hash === '#agents' && lane.ids.laneDetail.hidden, 'removed lane falls back to All');
  const audit = start('#auditSection', new Map(), '?token=fixture-auth'); await settle();
  const visibleAudit = h => h.ids.auditRows.children.filter(n => !n.hidden);
  check(visibleAudit(audit).length === 1 && !visibleAudit(audit)[0].textContent.includes('Read report') && !audit.ids.auditRows.children[0].querySelector('details').open, 'audit hides reads by default and keeps raw collapsed');
  check(audit.ids.auditReads?.getAttribute('type') === 'checkbox' && !audit.ids.auditReads.checked, 'Show reads is an unchecked labelled checkbox');
  check(/<label[^>]*>[\s\S]*?<input id="auditReads"[^>]*>\s*Show reads\s*<\/label>/.test(input.page), 'Show reads has its visible accessible label');
  check(audit.ids.auditCount.textContent === '1 of 2 entries', 'audit default count excludes reads');
  audit.ids.auditLane.value = 'codex-2'; vm.runInContext('filterAudit()', audit.context);
  check(visibleAudit(audit).length === 0 && !audit.ids.auditEmpty.hidden && audit.ids.auditCount.textContent === '0 of 2 entries', 'hidden read does not satisfy lane filter or count');
  audit.ids.auditReads.checked = true; audit.ids.auditReads.listeners.change();
  check(audit.ids.auditRows.children.filter(n => !n.hidden).length === 1, 'audit lane filter');
  const read = visibleAudit(audit)[0];
  check(read.children[5].textContent === 'n/a' && read.children[5].classList.contains('muted'), 'missing brief is muted n/a');
  audit.ids.auditAction.value = 'nuntius_dispatch'; vm.runInContext('filterAudit()', audit.context);
  check(audit.ids.auditRows.children.filter(n => !n.hidden).length === 0, 'audit combined action filter');
  audit.ids.auditLane.value = ''; audit.ids.auditAction.value = ''; audit.ids.auditFilter.value = 'DASHBOARD'; vm.runInContext('filterAudit()', audit.context);
  check(audit.ids.auditRows.children.filter(n => !n.hidden).length === 1, 'audit text filter');
  check(audit.ids.auditRows.children[0].querySelector('time').title.length > 0, 'audit exact local time on hover');
  audit.ids.auditFilter.value = ''; audit.ids.auditAction.value = 'nuntius_report';
  audit.ids.auditReads.checked = false; audit.ids.auditReads.listeners.change();
  check(visibleAudit(audit).length === 0 && !audit.ids.auditEmpty.hidden, 'read action filter respects Show reads');
  audit.ids.auditReads.checked = true; audit.ids.auditReads.listeners.change();
  check(visibleAudit(audit).length === 1, 'Show reads composes with action filter');
  const auditStore = new Map(), rememberedAudit = start('#auditSection', auditStore, '?token=fixture-auth'); await settle();
  rememberedAudit.ids.auditReads.checked = true; rememberedAudit.ids.auditReads.listeners.change();
  const restoredAudit = start('#auditSection', auditStore); await settle();
  check(restoredAudit.ids.auditReads.checked && visibleAudit(restoredAudit).length === 2, 'Show reads survives a fresh tab');
  restoredAudit.ids.auditReads.checked = false; restoredAudit.ids.auditReads.listeners.change();
  const hiddenAgain = start('#auditSection', auditStore); await settle();
  check(!hiddenAgain.ids.auditReads.checked && visibleAudit(hiddenAgain).length === 1, 'unchecked Show reads persists');
  const blockedAudit = start('#auditSection', new Map(), '?token=fixture-auth', true); await settle();
  blockedAudit.ids.auditReads.checked = true; blockedAudit.ids.auditReads.listeners.change();
  check(visibleAudit(blockedAudit).length === 2, 'Show reads works when storage is disabled');
  vm.runInContext("lastData = JSON.parse(JSON.stringify(lastData)); lastData.audit = [{time:'2026-10-10T00:00:00Z',action:'freeze',summary:'Froze dispatch',result:'ok',lane:'',goal_id:'',digest:'',raw:'{}'}, {time:'2026-10-10T00:00:01Z',action:'future_action',summary:'Unknown action',result:'refused',lane:'codex-2',goal_id:'',digest:'',raw:'{}'}]; el('auditAction').value = ''; render(lastData)", hiddenAgain.context);
  check(visibleAudit(hiddenAgain).length === 2 && hiddenAgain.ids.auditCount.textContent === '2 of 2 entries', 'unknown and state-changing actions remain visible without metadata');
  const allLanes = hiddenAgain.ids.auditRows.children[0];
  check(allLanes.children[2].textContent === 'all lanes' && allLanes.children[3].textContent === 'n/a' && allLanes.children[3].classList.contains('muted') && allLanes.children[5].textContent === 'n/a', 'audit empty scope goal and brief use meaningful labels');
  vm.runInContext("lastData.audit.push({time:'2026-10-10T00:00:02Z',action:'future_read',read_only:true,summary:'Future inspection',result:'ok',lane:'',goal_id:'',digest:'',raw:'{}'}); render(lastData)", hiddenAgain.context);
  check(hiddenAgain.ids.auditRows.children.length === 3 && visibleAudit(hiddenAgain).length === 2 && hiddenAgain.ids.auditCount.textContent === '2 of 3 entries', 'refresh retains reads and filters using server metadata rather than action names');
  hiddenAgain.ids.auditReads.checked = true; hiddenAgain.ids.auditReads.listeners.change();
  check(visibleAudit(hiddenAgain).length === 3 && hiddenAgain.ids.auditCount.textContent === '3 of 3 entries', 'Show reads reveals retained inspection after refresh');
  vm.runInContext("lastData.audit = []; render(lastData)", hiddenAgain.context);
  check(visibleAudit(hiddenAgain).length === 0 && !hiddenAgain.ids.auditEmpty.hidden && hiddenAgain.ids.auditCount.textContent === '0 of 0 entries', 'empty audit has a correct count and message');
  const unsigned = start('#runs'); await settle();
  check(unsigned.calls.length === 0 && !unsigned.ids.error.hidden && unsigned.ids.error.textContent === 'Open the link printed by \x60nuntius web\x60 to sign in.', 'empty tab storage shows sign-in instructions without data requests');
  await vm.runInContext("action('unfreeze')", unsigned.context);
  check(unsigned.calls.length === 0, 'unsigned action does not send API requests');
  const blocked = start('#runs', new Map(), '?token=fixture-auth', true); await settle();
  check(rendered(blocked, 'runs') && !blocked.location.search, 'storage failure permits current sign-in and clears URL');
  const noStorage = start('#runs', new Map(), '', true); await settle();
  check(noStorage.calls.length === 0 && !noStorage.ids.error.hidden, 'storage failure without bootstrap remains signed out');
  const replaced = new Map([['nuntius.web.credential', JSON.stringify({token:'stale-fixture-auth',expiresAt:1791658800000+sevenDays})]]);
  const fresh = start('#runs', replaced, '?token=fixture-auth'); await settle();
	vm.runInContext("selectTab('agents'); lastData.lanes[0].eta = 'over usual (+7m)'; lastData.lanes[0].eta_details = {explanation:'Usual = 24m: median of 3 earlier codex size-S runs; this run 31m'}; globalThis.alert = text => { globalThis.tappedExplanation = text; }; render(lastData)", fresh.context);
	const eta = fresh.ids.lanes.children[0].querySelectorAll('dd')[2];
	check(eta.textContent === 'over usual (+7m)' && eta.title.includes('median of 3 earlier codex size-S runs'), 'ETA preserves short label with hover provenance');
	eta.onclick(); check(vm.runInContext('tappedExplanation',fresh.context) === eta.title, 'phone tap explains ETA');
  vm.runInContext("selectTab('agents'); lastData = JSON.parse(JSON.stringify(lastData)); lastData.lanes[0].display_state = 'timed_out'; lastData.lanes[1].display_state = 'cancelled'; lastData.lanes[2].display_state = 'stuck?'; render(lastData)", fresh.context);
  check(fresh.ids.lanes.textContent.includes('Timed out') && fresh.ids.lanes.textContent.includes('Cancelled') && fresh.ids.lanes.textContent.includes('Stuck?'), 'distinct terminal and stuck card states');
  vm.runInContext("lastData.history[0].display_state = 'cancelled'; selectTab('runs')", fresh.context);
  check(fresh.ids.runs.textContent.includes('Cancelled'), 'history cancellation badge');
  vm.runInContext("lastData.history[0].display_state = 'timed_out'; render(lastData)", fresh.context);
  check(fresh.ids.runs.textContent.includes('Timed out'), 'history timeout badge');
  // Badge colour is the server's tone field; the label text is never parsed.
  const tones = [['Running · resumed 1/2', 'green'], ['Ended · resumed 2/2', 'green'], ['Failed · resumed 1/2', 'red'], ['Cancelled · resumed 1/2', 'amber'], ['Timed out · resumed 1/2', 'amber'], ['interrupted; waiting to resume', ''], ['interrupted', '']];
  for (const [label, tone] of tones) {
    const shown = label.charAt(0).toUpperCase() + label.slice(1), className = tone ? 'badge ' + tone : 'badge';
    vm.runInContext("selectTab('agents'); lastData.lanes[1].display_state = " + JSON.stringify(label) + "; lastData.lanes[1].badge = " + JSON.stringify(tone) + "; render(lastData)", fresh.context);
    const card = fresh.ids.lanes.children[1].querySelector('.badge');
    check(card.textContent === shown && card.className === className, 'card badge tone from server: ' + label);
    vm.runInContext("lastData.history[0].display_state = " + JSON.stringify(label) + "; lastData.history[0].badge = " + JSON.stringify(tone) + "; selectTab('runs'); render(lastData)", fresh.context);
    const row = fresh.ids.runs.querySelector('.badge');
    check(row.textContent === shown && row.className === className, 'history badge tone from server: ' + label);
  }
  vm.runInContext("selectTab('agents'); lastData.lanes[1].display_state = 'Running · resumed 1/2'; delete lastData.lanes[1].badge; render(lastData)", fresh.context);
  check(fresh.ids.lanes.children[1].querySelector('.badge').className === 'badge', 'label text alone never colours a badge');
  vm.runInContext("lastData.history[0].display_state = 'Ended · resumed 1/2'; lastData.history[0].badge = 'green'; lastData.history[0].resume_count = 1; lastData.history[0].relation = 'resumed 1/2 after interruption at 23:31; resumed by the 23:40 run'; lastData.history[0].chain_depth = 1; selectTab('runs'); render(lastData)", fresh.context);
  check(fresh.ids.runs.textContent.includes('Ended · resumed 1/2') && fresh.ids.runs.textContent.includes('resumed 1/2 after interruption at 23:31') && fresh.ids.runs.textContent.includes('resumed by the 23:40 run') && !fresh.ids.runs.textContent.includes('Previous run:'), 'history exposes readable recovery relations');
  check(fresh.ids.runs.querySelector('.chain-child') !== null, 'history indents descendants');
  check(fresh.ids.runs.textContent.includes(input.data.history[0].run_id), 'history shows the short run ID');
  vm.runInContext("selectTab('agents'); lastData.lanes[0].current_action = {summary: 'go test ./...', age_seconds: 42, age_source: 'observed', running_seconds: 42}; render(lastData)", fresh.context);
  check(fresh.ids.lanes.textContent.includes('go test ./... (~42s ago); running for ~42s'), 'cards show action and observed duration');
  check(fresh.ids.lanes.textContent.includes(input.data.lanes[0].run_id), 'cards show the same short run ID');
  vm.runInContext("render(lastData)", fresh.context);
  check(fresh.ids.freeze.textContent.includes('Enabled') && fresh.ids.freeze.title === 'Dispatch enabled' && fresh.ids.freeze.getAttribute('aria-label') === 'Dispatch enabled', 'short enabled label has full accessible text');
  vm.runInContext("lastData.freeze = {since: '2026-10-09T10:00:00Z', reason: 'Owner pause'}; render(lastData)", fresh.context);
  check(fresh.ids.freeze.textContent.includes('Frozen') && fresh.ids.freeze.title.includes('Owner pause') && fresh.ids.freeze.getAttribute('aria-label') === fresh.ids.freeze.title, 'short frozen label has full accessible text');
  check(JSON.parse(replaced.get('nuntius.web.credential')).token === 'fixture-auth', 'new start link replaces stale device credential');
  await vm.runInContext("action('unfreeze')", fresh.context);
  check(fresh.calls.some(c => c.path === '/action' && c.options.method === 'POST' && c.options.headers['X-Nuntius-Token'] === fresh.calls[0].options.headers['X-Nuntius-Token']), 'actions use the same tab credential header');
  fresh.doc.keydown({type: 'keydown', target: fresh.ids.runs});
  check(fresh.doc.documentElement.classList.contains('keyboard-focus'), 'keyboard input enables panel focus indicator');
  fresh.doc.pointerdown({type: 'pointerdown', target: fresh.ids.runs});
  check(!fresh.doc.documentElement.classList.contains('keyboard-focus'), 'pointer input suppresses panel focus indicator');
  check(!vm.runInContext("busy(el('runs'))", fresh.context), 'panel target itself does not create a hold');
})().catch(() => { process.stderr.write(predicate); process.exitCode = 1; });
`

// Badge tones resolve to theme variables that both colour schemes define, and
// the script never maps label text to a colour.
func TestDashboardBadgeTonesInBothThemes(t *testing.T) {
	css, err := dashboardAssets.ReadFile("app.css")
	if err != nil {
		t.Fatal(err)
	}
	script, err := dashboardAssets.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(script), "resumed [12]") || strings.Contains(string(script), "known.includes(state)") {
		t.Fatal("badge colour must come from the server badge field, not label text")
	}
	light, dark, found := strings.Cut(string(css), "@media(prefers-color-scheme:dark)")
	if !found {
		t.Fatal("dark theme missing")
	}
	for _, tone := range []string{"green", "amber", "red"} {
		if !strings.Contains(string(css), ".badge."+tone+"{color:var(--"+tone+");background:var(--"+tone+"-soft)}") {
			t.Fatalf("badge tone rule missing: %s", tone)
		}
		for _, name := range []string{"--" + tone + ":", "--" + tone + "-soft:"} {
			if !strings.Contains(light, name) || !strings.Contains(dark, name) {
				t.Fatalf("theme variable missing: %s", name)
			}
			lightValue, _, _ := strings.Cut(strings.SplitN(light, name, 2)[1], ";")
			darkValue, _, _ := strings.Cut(strings.SplitN(dark, name, 2)[1], ";")
			if lightValue == darkValue {
				t.Fatalf("theme variable identical in both schemes: %s", name)
			}
		}
	}
}

// Strict decoding binds the screenshot fixture to actual card/status types and
// the existing history, notes and freeze JSON shape, including optional fields.
func TestDashboardFixtureMatchesData(t *testing.T) {
	type historyView struct {
		Kind          string            `json:"kind"`
		Model         string            `json:"model"`
		Effort        string            `json:"effort"`
		Relation      string            `json:"relation"`
		ChainID       string            `json:"chain_id"`
		ChainDepth    int               `json:"chain_depth"`
		RunID         string            `json:"run_id"`
		Lane          string            `json:"lane"`
		GoalID        string            `json:"goal_id"`
		Started       time.Time         `json:"started"`
		Ended         *time.Time        `json:"ended"`
		State         string            `json:"state"`
		DisplayState  string            `json:"display_state"`
		Exit          *int              `json:"exit"`
		Goal          string            `json:"goal"`
		Report        string            `json:"report"`
		Sections      map[string]string `json:"sections"`
		ResumeCount   int               `json:"resume_count"`
		Badge         string            `json:"badge"`
		ParentRunID   string            `json:"parent_run_id"`
		ContinueRunID string            `json:"continue_run_id"`
	}
	type dashboard struct {
		Freeze    *FreezeState        `json:"freeze"`
		Lanes     []webLane           `json:"lanes"`
		Tails     map[string][]string `json:"tails"`
		History   []historyView       `json:"history"`
		Notes     []OwnerNote         `json:"notes"`
		Audit     []AuditEntry        `json:"audit"`
		Signals   map[string][]string `json:"signals"`
		Oversight string              `json:"oversight"`
	}
	decode := func(b []byte) dashboard {
		t.Helper()
		var d dashboard
		decoder := json.NewDecoder(bytes.NewReader(b))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&d) != nil {
			t.Fatal("dashboard fixture/data shape differs")
		}
		for _, lane := range d.Lanes {
			if lane.Kind == "" || lane.ETA == "" || lane.Activity == "" {
				t.Fatal("missing presentation field")
			}
		}
		return d
	}
	sample, err := os.ReadFile("testdata/dashboard-data.json")
	if err != nil {
		t.Fatal(err)
	}
	fixture := decode(sample)
	if len(fixture.Lanes) != 4 || len(fixture.History) == 0 || len(fixture.Notes) == 0 || fixture.Oversight == "" {
		t.Fatal("unrealistic fixture")
	}
	states := map[string]bool{}
	for _, lane := range fixture.Lanes {
		states[lane.State] = true
	}
	if len(states) != 4 {
		t.Fatal("fixture lacks varied states")
	}
	for _, h := range fixture.History {
		expected := reportSections(h.Report)
		if expected["QUESTIONS"] != h.Sections["QUESTIONS"] || expected["DECISIONS"] != h.Sections["DECISIONS"] {
			t.Fatal("fixture sections differ from real extraction")
		}
	}
	if Redact(string(sample)) != string(sample) {
		t.Fatal("fixture is not redacted")
	}
	m, _, _ := manager(t)
	if _, err = m.Dispatch("codex", "SHAPE-1", "hello"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://localhost:8123/data", nil)
	req.Header.Set("X-Nuntius-Token", w.token)
	out := httptest.NewRecorder()
	w.ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatal("data unavailable")
	}
	real := decode(out.Body.Bytes())
	if len(real.History) == 0 || len(real.Lanes) == 0 {
		t.Fatal("real shape not exercised")
	}
	var actualKeys, sampleKeys map[string]json.RawMessage
	if json.Unmarshal(out.Body.Bytes(), &actualKeys) != nil || json.Unmarshal(sample, &sampleKeys) != nil {
		t.Fatal("shape decode")
	}
	if len(actualKeys) != len(sampleKeys) {
		t.Fatal("fixture root keys differ")
	}
	for key := range actualKeys {
		if _, ok := sampleKeys[key]; !ok {
			t.Fatal("fixture root field missing")
		}
	}
}
