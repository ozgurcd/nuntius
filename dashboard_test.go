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
	if strings.Count(html, `role="tablist"`) != 1 || strings.Count(html, `role="tab"`) != 5 || strings.Count(html, `role="tabpanel"`) != 5 || strings.Count(html, `aria-selected="true"`) != 1 {
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
	for _, contract := range []string{"selectTab(location.hash.slice(1))", "'/' + location.hash", "'hashchange'", "'ArrowLeft'", "'ArrowRight'", "'Home'", "'End'", "panel.hidden = !selected", "tab.setAttribute('aria-selected', String(selected))", "tab.tabIndex = selected ? 0 : -1"} {
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
	input, err := json.Marshal(map[string]any{"data": json.RawMessage(fixture), "script": string(script), "page": string(page)})
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
  constructor(tag) { this.tagName = tag.toUpperCase(); this.children = []; this.attrs = {}; this.value = ''; this.hidden = false; this.open = false; this.dataset = {}; this.listeners = {}; this.className = ''; this.text = ''; }
  get textContent() { return this.text + this.children.map(n => n.textContent).join(''); }
  set textContent(value) { this.replaceChildren(); this.text = String(value); }
  get firstElementChild() { return this.children[0] || null; }
  get nextElementSibling() { const siblings = this.parentElement.children; return siblings[siblings.indexOf(this) + 1] || null; }
  get options() { return this.children; }
  get classList() { return {add: c => { this.className += ' ' + c; }, remove: c => { this.className = this.className.split(' ').filter(x => x !== c).join(' '); }, contains: c => this.className.split(' ').includes(c)}; }
  setAttribute(key, value) { this.attrs[key] = String(value); if (key === 'id') this.id = value; if (key === 'class') this.className = value; if (key === 'hidden') this.hidden = true; }
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
  focus() { this.document.activeElement = this; }
}
function start(hash, store = new Map(), query = '', blocked = false) {
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
  const location = {href: 'http://localhost:8123/' + query + hash, hash, search: query};
  const context = vm.createContext({document: doc, window: win, location, URL, history: {replaceState: (_, __, path) => { location.href = 'http://localhost:8123' + path; location.hash = path.includes('#') ? path.slice(path.indexOf('#')) : ''; location.search = ''; }}, sessionStorage: {getItem: k => { if (blocked) throw new Error(); return store.get(k) || null; }, setItem: (k, v) => { if (blocked) throw new Error(); store.set(k, v); }}, fetch: async (path, options) => { calls.push({path, options}); return {ok: true, json: async () => input.data}; }, setInterval: fn => { timers.push(fn); }, setTimeout: () => 1, clearTimeout: () => {}});
  vm.runInContext(input.script, context);
  return {context, ids, calls, timers, doc, win, location};
}
async function settle() { for (let i = 0; i < 8; i++) await Promise.resolve(); }
function rendered(h, id) {
  if (id === 'runs') return h.ids.runs.querySelectorAll('tbody').length === input.data.history.length;
  if (id === 'messages') return h.ids.notes.children.length === input.data.notes.length && h.ids.lane.options.length === input.data.lanes.length;
  if (id === 'digest') return h.ids.oversight.children.length > 0;
  return h.ids.audit.textContent === input.data.audit;
}
(async () => {
  for (const id of ['runs', 'messages', 'digest', 'auditSection']) {
    predicate = 'hash-load ' + id;
    const h = start('#' + id, new Map(), '?token=fixture-auth');
    if (id === 'auditSection') h.ids.auditSection.querySelector('details').open = true;
    await settle();
    check(rendered(h, id), 'focused hash panel renders: ' + id);
    check(h.calls.length === 1 && h.calls[0].path === '/data', 'authenticated initial data fetch');
    check(!h.location.search, 'bootstrap removes URL query');
    const store = new Map();
    const first = start('#agents', store, '?token=fixture-auth'); await settle();
    const reloaded = start('#' + id, store); await settle();
    check(rendered(reloaded, id), 'reload retains hash and authentication: ' + id);
    check(reloaded.calls.length === 1 && reloaded.calls[0].options.headers['X-Nuntius-Token'] === first.calls[0].options.headers['X-Nuntius-Token'], 'reload uses tab storage');
    // A hidden panel can retain focus and a recent hold from a prior interaction.
    vm.runInContext("holds.set(el('" + id + "'), Date.now() + 5000)", h.context);
    vm.runInContext("selectTab('agents')", h.context);
    vm.runInContext("lastData = JSON.parse(JSON.stringify(lastData)); lastData.notes[0].text = 'Updated owner note'; lastData.oversight = 'Updated digest'; lastData.audit = 'Updated audit'; lastData.history = lastData.history.slice(0, 1); render(lastData)", h.context);
    if (id === 'auditSection') h.ids.auditSection.querySelector('details').open = false;
    if (id === 'runs') h.ids.filter.value = input.data.history[0].goal_id;
    vm.runInContext("selectTab('" + id + "')", h.context);
    const updated = id === 'runs' ? h.ids.runs.querySelectorAll('tbody').length === 1 : id === 'messages' ? h.ids.notes.textContent.includes('Updated owner note') : id === 'digest' ? h.ids.oversight.textContent.includes('Updated digest') : h.ids.audit.textContent === 'Updated audit';
    check(updated, 'busy panel receives changed data when made visible: ' + id);
    if (id === 'runs') check(h.ids.runs.querySelectorAll('tbody').filter(n => !n.hidden).length === 1, 'history applies deferred filter on visibility');
    check(vm.runInContext("busy(el('" + id + "'))", h.context), 'recent interaction still pauses subsequent refresh');
    vm.runInContext("holds.delete(el('" + id + "'))", h.context);
    check(!vm.runInContext("busy(el('" + id + "'))", h.context), 'panel element focus alone is not interaction');
    h.ids.filter.focus();
    check(vm.runInContext("busy(el('runs'))", h.context), 'input focus remains protected');
  }
  const unsigned = start('#runs'); await settle();
  check(unsigned.calls.length === 0 && !unsigned.ids.error.hidden && unsigned.ids.error.textContent === 'Open the link printed by \x60nuntius web\x60 to sign in.', 'empty tab storage shows sign-in instructions without data requests');
  await vm.runInContext("action('unfreeze')", unsigned.context);
  check(unsigned.calls.length === 0, 'unsigned action does not send API requests');
  const blocked = start('#runs', new Map(), '?token=fixture-auth', true); await settle();
  check(rendered(blocked, 'runs') && !blocked.location.search, 'storage failure permits current sign-in and clears URL');
  const noStorage = start('#runs', new Map(), '', true); await settle();
  check(noStorage.calls.length === 0 && !noStorage.ids.error.hidden, 'storage failure without bootstrap remains signed out');
  const replaced = new Map([['nuntius.web.token', 'stale-fixture-auth']]);
  const fresh = start('#runs', replaced, '?token=fixture-auth'); await settle();
  vm.runInContext("selectTab('agents'); lastData = JSON.parse(JSON.stringify(lastData)); lastData.lanes[0].display_state = 'timed_out'; lastData.lanes[1].display_state = 'cancelled'; lastData.lanes[2].display_state = 'stuck?'; render(lastData)", fresh.context);
  check(fresh.ids.lanes.textContent.includes('Timed out') && fresh.ids.lanes.textContent.includes('Cancelled') && fresh.ids.lanes.textContent.includes('Stuck?'), 'distinct terminal and stuck card states');
  vm.runInContext("lastData.history[0].display_state = 'cancelled'; selectTab('runs')", fresh.context);
  check(fresh.ids.runs.textContent.includes('Cancelled'), 'history cancellation badge');
  vm.runInContext("lastData.history[0].display_state = 'timed_out'; render(lastData)", fresh.context);
  check(fresh.ids.runs.textContent.includes('Timed out'), 'history timeout badge');
  vm.runInContext("selectTab('agents'); lastData.lanes[0].display_state = 'interrupted; waiting to resume'; lastData.lanes[1].display_state = 'resumed (1/2)'; render(lastData)", fresh.context);
  check(fresh.ids.lanes.textContent.includes('waiting to resume') && fresh.ids.lanes.textContent.includes('Resumed (1/2)'), 'cards expose interrupted waiting and recovery count');
  vm.runInContext("lastData.history[0].display_state = 'resumed (1/2)'; lastData.history[0].resume_count = 1; lastData.history[0].parent_run_id = 'previous-fixture-run'; lastData.history[0].continue_run_id = 'next-fixture-run'; selectTab('runs')", fresh.context);
  check(fresh.ids.runs.textContent.includes('Resumed (1/2)') && fresh.ids.runs.textContent.includes('Previous run: previous-fixture-run') && fresh.ids.runs.textContent.includes('Next run: next-fixture-run') && fresh.ids.runs.textContent.includes('Recovery: 1/2'), 'history exposes the recovery chain');
  vm.runInContext("render(lastData)", fresh.context);
  check(fresh.ids.freeze.textContent.includes('Enabled') && fresh.ids.freeze.title === 'Dispatch enabled' && fresh.ids.freeze.getAttribute('aria-label') === 'Dispatch enabled', 'short enabled label has full accessible text');
  vm.runInContext("lastData.freeze = {since: '2026-10-09T10:00:00Z', reason: 'Owner pause'}; render(lastData)", fresh.context);
  check(fresh.ids.freeze.textContent.includes('Frozen') && fresh.ids.freeze.title.includes('Owner pause') && fresh.ids.freeze.getAttribute('aria-label') === fresh.ids.freeze.title, 'short frozen label has full accessible text');
  check(replaced.get('nuntius.web.token') === 'fixture-auth', 'new start link replaces stale tab credential');
  await vm.runInContext("action('unfreeze')", fresh.context);
  check(fresh.calls.some(c => c.path === '/action' && c.options.method === 'POST' && c.options.headers['X-Nuntius-Token'] === fresh.calls[0].options.headers['X-Nuntius-Token']), 'actions use the same tab credential header');
  fresh.doc.keydown({type: 'keydown', target: fresh.ids.runs});
  check(fresh.doc.documentElement.classList.contains('keyboard-focus'), 'keyboard input enables panel focus indicator');
  fresh.doc.pointerdown({type: 'pointerdown', target: fresh.ids.runs});
  check(!fresh.doc.documentElement.classList.contains('keyboard-focus'), 'pointer input suppresses panel focus indicator');
  check(!vm.runInContext("busy(el('runs'))", fresh.context), 'panel target itself does not create a hold');
})().catch(() => { process.stderr.write(predicate); process.exitCode = 1; });
`

// Strict decoding binds the screenshot fixture to actual card/status types and
// the existing history, notes and freeze JSON shape, including optional fields.
func TestDashboardFixtureMatchesData(t *testing.T) {
	type historyView struct {
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
		ParentRunID   string            `json:"parent_run_id"`
		ContinueRunID string            `json:"continue_run_id"`
	}
	type dashboard struct {
		Freeze    *FreezeState        `json:"freeze"`
		Lanes     []webLane           `json:"lanes"`
		Tails     map[string][]string `json:"tails"`
		History   []historyView       `json:"history"`
		Notes     []OwnerNote         `json:"notes"`
		Audit     string              `json:"audit"`
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
