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
	panels := []string{"agents", "queue", "messages", "digest", "runs", "auditSection", "settings"}
	if strings.Count(html, `role="tablist"`) != 2 || strings.Count(html, `role="tab"`) != len(panels) || strings.Count(html, `role="tabpanel"`) != len(panels) || strings.Count(html, `aria-selected="true"`) != 1 {
		t.Fatal("dashboard needs every accessible tab and one initial selection")
	}
	for _, id := range panels {
		if !strings.Contains(html, `id="tab-`+id+`"`) || !strings.Contains(html, `aria-controls="`+id+`"`) || !strings.Contains(html, `aria-labelledby="tab-`+id+`"`) {
			t.Fatal("tab and panel association missing")
		}
	}
	if !strings.Contains(html, `id="tab-agents" role="tab" aria-selected="true" tabindex="0"`) || strings.Count(html, `role="tabpanel" hidden`) != len(panels) || !strings.Contains(html, `id="signInPanel"`) {
		t.Fatal("public bootstrap must show sign-in and hide every private panel")
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

func TestDashboardPollingRuntime(t *testing.T) {
	for _, name := range []string{"fetch_deadline", "body_deadline", "foreground_visible", "foreground_focus", "foreground_pageshow", "ownership", "failure_age", "settings_draft", "read_cancellation", "settings_save_guard"} {
		paths := []string{""}
		if strings.Contains(name, "deadline") {
			paths = []string{"/queue", "/data", "/settings"}
		}
		for _, path := range paths {
			t.Run(name+"/"+strings.TrimPrefix(path, "/"), func(t *testing.T) {
				assets := map[string]any{"test": name, "path": path}
				for key, path := range map[string]string{"script": "app.js", "page": "index.html", "css": "app.css"} {
					data, err := dashboardAssets.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					assets[key] = string(data)
				}
				data, err := os.ReadFile("testdata/dashboard-data.json")
				if err != nil {
					t.Fatal(err)
				}
				assets["data"] = json.RawMessage(data)
				input, err := json.Marshal(assets)
				if err != nil {
					t.Fatal("cannot encode polling fixture")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				// Share the existing offline DOM/transport harness, not its test sequence.
				prefix := dashboardRuntimeTest[:strings.Index(dashboardRuntimeTest, "(async () => {")]
				cmd := exec.CommandContext(ctx, "node", "-e", prefix+dashboardPollingTest)
				cmd.Stdin = bytes.NewReader(input)
				var diagnostic bytes.Buffer
				cmd.Stderr = &diagnostic
				if err := cmd.Run(); err != nil {
					t.Fatalf("offline polling predicate failed: %v (%s)", err, diagnostic.String())
				}
			})
		}
	}
}

const dashboardPollingTest = `
const deferred = () => { let resolve,reject; const promise=new Promise((a,b)=>{resolve=a;reject=b;}); return {promise,resolve,reject}; };
const reply = (data,status=200) => ({ok:status===200,status,json:async()=>data});
const read = (h,path,force=false) => vm.runInContext(path==='/data'?'refresh('+force+')':path==='/queue'?'loadQueue('+force+')':'loadSettings('+force+')',h.context);
const poll = async h => { h.timers[1](); h.timers[2](); await settle(); };
const expire = async (h,clock) => {
  clock.now+=10000;
  for(const [id,timer] of [...h.timeouts]) if(timer.delay===10000){h.timeouts.delete(id);timer.fn();}
  await settle();
};
const make = async (tab='queue') => {const clock={now:Date.parse(queueExample.updated)+60000};const h=start('#'+tab,new Map(),'?token=fixture-auth',false,clock);await settle();return {h,clock};};
(async()=>{
  if(input.test==='fetch_deadline'||input.test==='body_deadline'){
    for(const path of [input.path]){
      const {h,clock}=await make(path==='/settings'?'settings':'queue'), pending=deferred(); let calls=0,signal;
      const data=path==='/data'?input.data:path==='/queue'?{...queueExample,goals:[queueExample.goals[0]]}:exampleSettings;
      h.context.fetch=async(target,options)=>{if(target!==path)return reply(input.data);calls++;signal=options.signal;return calls===1?(input.test==='body_deadline'?{ok:true,status:200,json:()=>pending.promise}:pending.promise):reply(data);};
      read(h,path,true);await settle();await expire(h,clock);
      check(signal?.aborted,'P01 deadline aborts '+path+' '+input.test);
      await poll(h);if(path==='/settings')await read(h,path);await settle();
      check(calls===2,'P02 next poll runs after uncooperative stall '+path);
      if(path==='/queue')check(h.ids.queueTabCount.textContent==='1','P03 stalled Queue count recovers without reload');
      pending.resolve(input.test==='body_deadline'?data:reply(data));await settle();
    }
  }
  if(input.test.startsWith('foreground_')){
    for(const tab of ['queue','settings']){
      const {h}=await make(tab);const before=h.calls.length;
      if(input.test==='foreground_visible'){h.doc.hidden=true;h.doc.visibilitychange();h.doc.hidden=false;h.doc.visibilitychange();}
      if(input.test==='foreground_focus')h.win.focus();
      if(input.test==='foreground_pageshow')h.win.pageshow?.({persisted:true});
      await settle();
      check(h.calls.length===before+2 && h.calls.slice(before).some(c=>c.path==='/'+tab),'P04 immediate foreground data and selected '+tab+' '+input.test);
      h.win.focus();h.doc.visibilitychange();h.win.pageshow?.({persisted:true});await settle();
      check(h.calls.length===before+4,'P05 one foreground burst coalesces to one read per endpoint');
      h.doc.hidden=true;const count=h.calls.length;h.win.focus();h.win.pageshow?.({persisted:true});await settle();check(h.calls.length===count,'P06 hidden lifecycle skips reads');
    }
  }
  if(input.test==='ownership'){
    for(const path of ['/queue','/data','/settings'])for(const ending of ['response','body','error','403']){
      const {h}=await make(path==='/settings'?'settings':'queue'),old=deferred(),newest=deferred();let calls=0;
      const data=path==='/queue'?{...queueExample,goals:[queueExample.goals[0]]}:path==='/data'?{...input.data,lanes:[]}:exampleSettings;
      h.context.fetch=async target=>{if(target!==path)return reply(input.data);calls++;if(calls===1)return ending==='body'?{ok:true,status:200,json:()=>old.promise}:old.promise;return newest.promise;};
      read(h,path,true);await settle();h.win.focus();await settle();
      check(calls===2,'P07 foreground supersedes pending '+path);
      if(ending==='error')old.reject(new Error());else old.resolve(ending==='body'?data:reply(data,ending==='403'?403:200));await settle();
      check(vm.runInContext('authenticated()',h.context),'P08 old rejection cannot sign out');
      if(path==='/queue')check(h.ids.queueTabCount.textContent==='3' && h.ids.queueBanner.hidden,'P08 old Queue result or error cannot alter display');
      if(path==='/data')check(Number(h.ids.runningCount.textContent)>0 && h.ids.error.hidden,'P08 old data result or error cannot alter display');
      if(path==='/settings')check(vm.runInContext('settingsConfirmed',h.context) && h.ids.settingsBanner.hidden,'P08 old settings error cannot alter display');
      check(vm.runInContext(path==='/queue'?'queueLoading':path==='/data'?'refreshing':'settingsLoading',h.context),'P09 old finally cannot release newer owner');
      newest.resolve(reply(data));await settle();
      if(path==='/queue')check(h.ids.queueTabCount.textContent==='1','P10 newest Queue owns count');
      if(path==='/data')check(h.ids.runningCount.textContent==='0','P10 newest data owns header');
      if(path==='/settings')check(vm.runInContext('settingsConfirmed',h.context),'P10 newest settings confirmed');
      // A late body after the replacement has committed cannot roll it back.
      const late=deferred();h.context.fetch=async target=>target===path?{ok:true,status:200,json:()=>late.promise}:reply(input.data);
      read(h,path,true);await settle();h.context.fetch=async target=>reply(target===path?data:input.data);h.win.focus();await settle();late.resolve(path==='/queue'?queueExample:path==='/data'?input.data:{...exampleSettings,revision:'old'});await settle();
      if(path==='/queue')check(h.ids.queueTabCount.textContent==='1','P11 out-of-order body ignored');
      if(path==='/data')check(h.ids.runningCount.textContent==='0','P11 out-of-order data ignored');
      if(path==='/settings')check(vm.runInContext('settingsCurrent.revision',h.context)==='example-revision','P11 out-of-order settings ignored');
    }
  }
  if(input.test==='failure_age'){
    const {h,clock}=await make();h.context.fetch=async()=>{throw new Error();};
    for(let i=0;i<3;i++){clock.now+=10000;await read(h,'/queue');}
    check(h.ids.queueTabCount.textContent==='3' && !h.ids.queueBanner.hidden && /30s ago/.test(h.ids.queueBanner.textContent),'P12 repeated failure shows last-success age');
    h.context.fetch=async()=>reply(queueExample);await read(h,'/queue');check(h.ids.queueBanner.hidden,'P13 success clears stale status');
    const s=await make('settings');s.clock.now+=30000;s.h.context.fetch=async()=>reply({code:'configuration_unavailable'},503);await read(s.h,'/settings',true);
    check(/30s ago/.test(s.h.ids.settingsBanner.textContent),'P12 settings HTTP failure shows last-success age');
  }
  if(input.test==='settings_draft'){
    const {h,clock}=await make('settings');h.ids['setting-codex'].value='3';h.ids['setting-codex'].listeners.input();const pending=deferred();
    h.context.fetch=async target=>target==='/settings'?pending.promise:reply(input.data);read(h,'/settings',true);await settle();await expire(h,clock);
    check(h.ids['setting-codex'].value==='3' && h.ids.settingsSave.disabled,'P14 timeout keeps draft and requires confirmed read');
    h.context.fetch=async target=>reply(target==='/settings'?exampleSettings:input.data);h.win.focus();await settle();check(h.ids['setting-codex'].value==='3' && !h.ids.settingsSave.disabled,'P15 foreground confirms without dropping draft');
  }
  if(input.test==='read_cancellation'){
    for(const path of ['/queue','/data','/settings'])for(const stage of ['fetch','body'])for(const ending of ['signout','expiry','pagehide']){
      const {h,clock}=await make(path==='/settings'?'settings':'queue'),pending=deferred();let signal;
      h.context.fetch=async(target,options)=>{signal=options.signal;return stage==='body'?{ok:true,status:200,json:()=>pending.promise}:pending.promise;};
      read(h,path,true);await settle();if(ending==='signout')h.ids.signOut.click();else if(ending==='expiry'){clock.now+=7*24*60*60*1000;vm.runInContext('authenticated()',h.context);}else h.win.pagehide?.();await settle();
      check(signal?.aborted,'P16 '+ending+' aborts '+path+' '+stage);
      pending.resolve(stage==='body'?queueExample:reply(queueExample));await settle();
      if(ending!=='pagehide')check(h.ids.queueTabCount.textContent==='—' && !vm.runInContext('authenticated()',h.context),'P17 late reply stays signed out');
      else check(!vm.runInContext(path==='/queue'?'queueLoading':path==='/data'?'refreshing':'settingsLoading',h.context),'P17 pagehide releases request');
    }
  }
  if(input.test==='settings_save_guard'){
    const {h}=await make('settings');h.ids['setting-codex'].value='3';h.ids['setting-codex'].listeners.input();const pending=deferred();let posts=0,gets=0;
    h.context.fetch=async(path,options)=>{if(options.method==='POST'){posts++;return pending.promise;}if(path==='/settings')gets++;return reply(input.data);};
    const save=vm.runInContext('saveSettings()',h.context);await settle();h.win.focus();h.win.pageshow?.({persisted:true});await settle();await poll(h);
    check(posts===1 && gets===0,'P18 no POST retry or read during save');pending.resolve(reply(exampleSettings));await save;
  }
})().catch(()=>{process.stderr.write(predicate);process.exitCode=1;});
`

const dashboardRuntimeTest = `
const vm = require('node:vm');
const input = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
let predicate = 'harness initialization';
function check(ok, label) { predicate = label; if (!ok) throw new Error(); }
const exampleSettings={schema_version:1,revision:'example-revision',format:'pools',pools:{claude:{size:1,configured:true,editable:true},codex:{size:2,configured:true,editable:true},opencode:{size:0,configured:false,editable:false}},notify_imessage_enabled:true,notify_effective:true,recipient_configured:true,max_runtime:'8h',auto_resume:true,notifications:true,reports_dir:'/example/reports',oversight_path:'',lanes:[{name:'codex pool template',kind:'codex',model:'default',effort:'default',workdir:'/example/workspace',binary:'/example/bin/fake',start_mode:'as_is',extra_args:'Configured locally',continuation_configured:true}],lane_names:['claude-1','codex-1','codex-2'],at_login:'Unavailable · check locally',tailnet_default:'Off · launch option',local_only_reason:'Edit locally; run nuntius config check'};
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
  removeAttribute(key) { delete this.attrs[key]; if (key === 'datetime') delete this.dateTime; }
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
const queueExample={status:'ok',version:1,updated:'2026-10-10T22:32:00Z',goals:[
 {id:'B',title:'<b>text only</b>',size:'M',lane_kinds:['codex','claude'],lane:'codex-1',order:4,state:'blocked',needs_owner:true,owner_reason:'Make a decision',depends_on:['A','EXTERNAL'],reason:'Waiting',created:'2026-10-10T22:00:00Z',updated:'2026-10-10T22:01:00Z'},
 {id:'A',title:'First goal',size:'L',lane_kinds:['opencode'],order:4,state:'ready',needs_owner:false,depends_on:[],created:'2026-10-10T22:00:00Z',updated:'2026-10-10T22:01:00Z'},
 {id:'C',title:'Planned owner hold',size:'S',lane_kinds:['claude'],order:8,state:'planned',needs_owner:true,owner_reason:'Approve scope',depends_on:[],created:'2026-10-10T22:00:00Z',updated:'2026-10-10T22:01:00Z'}]};
function start(hash, store = new Map(), query = '', blocked = false, clock = {now: 1791658800000}, status = 200, pending = null, pendingAction = null, settingsControl = {status:200,current:exampleSettings}, queueControl = {status:200,current:queueExample}) {
  const root = new Element('html'), ids = {}, stack = [root];
  // Parse the real static shell's element tree; no HTML renderer is involved.
  for (const match of input.page.matchAll(/<\/?([a-z][a-z0-9]*)([^>]*)>/gi)) {
    if (match[0].startsWith('</')) { if (stack.length > 1) stack.pop(); continue; }
    const n = new Element(match[1]);
    for (const a of match[2].matchAll(/([\w-]+)(?:="([^"]*)")?/g)) n.setAttribute(a[1], a[2] || '');
    if (n.id) ids[n.id] = n;
    stack.at(-1).append(n);
    if (!['area','base','br','col','embed','hr','img','input','link','meta','param','source','track','wbr'].includes(match[1].toLowerCase())) stack.push(n);
  }
  const doc = {hidden: false, documentElement: root, activeElement: ids[hash.slice(1)] || root, currentScript: {src: 'http://localhost:8123/app.js' + query}, getElementById: id => ids[id], querySelectorAll: s => root.querySelectorAll(s), createElement: tag => { const n = new Element(tag); n.document = doc; return n; }, addEventListener: (n, fn) => { doc[n] = fn; }};
  function attach(n) { n.document = doc; n.children.forEach(attach); } attach(root);
  const calls = [], timers = [], win = {getSelection: () => ({isCollapsed: true}), addEventListener: (n, fn) => { win[n] = fn; }};
  const location = {href: 'http://localhost:8123/' + query + hash, hash, search: query, reloads: 0, reload: () => { location.reloads++; }};
  const storage = {getItem: k => { if (blocked) throw new Error(); return store.get(k) || null; }, setItem: (k, v) => { if (blocked) throw new Error(); store.set(k, v); }, removeItem: k => { if (blocked) throw new Error(); store.delete(k); }};
  // Each start is a fresh tab: only origin localStorage survives discard.
  const session = new Map();
  class ClockDate extends Date { static now() { return clock.now; } }
  const context = vm.createContext({Date: ClockDate, document: doc, window: win, location, URL, history: {pushState: (_, __, path) => { location.hash = path.slice(path.indexOf('#')); }, replaceState: (_, __, path) => { location.href = 'http://localhost:8123' + path; location.hash = path.includes('#') ? path.slice(path.indexOf('#')) : ''; location.search = ''; }}, localStorage: storage, sessionStorage: {getItem: k => session.get(k) || null, setItem: (k,v) => session.set(k,v), removeItem: k => session.delete(k)}, fetch: async (path, options) => { calls.push({path, options}); if(path==='/queue'){if(queueControl.pending)return await queueControl.pending;if(queueControl.reject)throw new Error();return {ok:queueControl.status===200,status:queueControl.status,json:async()=>JSON.parse(JSON.stringify(queueControl.current))};} if(path==='/settings'){if(options.method==='POST' && settingsControl.post)return await settingsControl.post; if(settingsControl.reject)throw new Error(); return {ok:settingsControl.status===200,status:settingsControl.status,json:async()=>JSON.parse(JSON.stringify(settingsControl.current))};} if (path === '/action' && pendingAction) return await pendingAction; if (pending) await pending; return {ok: status === 200, status, json: async () => input.data}; }, setInterval: fn => { timers.push(fn); }, setTimeout: () => 1, clearTimeout: () => {}});
  const timeouts = new Map(); let timeoutID = 0;
  context.setTimeout = (fn, delay) => { const id = ++timeoutID; timeouts.set(id,{fn,delay}); return id; };
  context.clearTimeout = id => timeouts.delete(id);
  context.AbortController = AbortController;
  vm.runInContext(input.script, context);
  return {context, ids, calls, timers, timeouts, doc, win, location, settingsControl,queueControl};
}
async function settle() { for (let i = 0; i < 32; i++) await Promise.resolve(); }
function rendered(h, id) {
  if (id === 'runs') return h.ids.runs.querySelectorAll('tbody').length === input.data.history.length;
  if (id === 'messages') return h.ids.notes.children.length === input.data.notes.length && h.ids.lane.options.length === input.data.lanes.length;
  if (id === 'digest') return h.ids.oversight.children.length > 0;
  return h.ids.auditRows.children.length === input.data.audit.length;
}
(async () => {
  const tabIDs=['agents','queue','messages','digest','runs','auditSection','settings'];
  // Reload deliberately leaves this DOM intact: clearing must happen before
  // navigation completes, including while an action is waiting for a reply.
  const headerFailures = [];
  const publicHeader = start('#queue'); await settle();
  const headerSnapshot = h => {
    const read = n => ({tag:n.tagName, text:n.textContent, classes:n.className.split(/\s+/).filter(Boolean).sort(),
      attrs:Object.entries(n.attrs).filter(([key]) => !['class','hidden','title','datetime'].includes(key)).sort(),
      data:Object.entries(n.dataset).sort(), title:n.title || '', dateTime:n.dateTime || '', hidden:n.hidden, children:n.children.map(read)});
    return JSON.stringify(read(h.doc.querySelectorAll('header')[0]));
  };
  const neutralHeader = headerSnapshot(publicHeader);
  for (const trigger of ['sign-out', 'expiry', 'cross-tab', 'server-rejection']) for (const frozen of [false, true]) for (const pendingKind of ['none','action','data-response','data-json','queue-response','queue-json']) {
    const actionPending = pendingKind === 'action';
    const label = trigger + ' ' + (frozen ? 'frozen' : 'enabled') + ' ' + pendingKind;
    const clock = {now:1791658800000}, store = new Map();
    let finishAction;
    const response = new Promise(resolve => { finishAction = resolve; });
    const h = start('#queue', store, '?token=fixture-auth', false, clock, 200, null, actionPending ? response : null); await settle();
    vm.runInContext("lastData = JSON.parse(JSON.stringify(lastData)); lastData.freeze = {since:'2026-10-11T00:00:00Z',reason:'Private fixture reason'}; lastData.lanes = [{lane:'fixture-a',state:'running'},{lane:'fixture-b',state:'running'}]; lastData.notes = [{id:'a',read:false},{id:'b',read:false}]; lastData.history = [{sections:{QUESTIONS:'Fixture question A'}},{sections:{QUESTIONS:'Fixture question B'}}]; render(lastData)", h.context);
    if (!frozen) vm.runInContext('lastData.freeze = null; render(lastData)', h.context);
    const headerCounts = ['agentsTabCount','notesTabCount','questionsTabCount','queueTabCount'];
    check(h.ids.freeze.classList.contains(frozen ? 'red' : 'green') && (frozen ? h.ids.freeze.title.includes('Private fixture reason') : h.ids.freeze.title === 'Dispatch enabled') && h.ids.freeze.getAttribute('aria-label') === h.ids.freeze.title && headerCounts.every(id => Number(h.ids[id].textContent) > 0) && !!h.ids.lastRefresh.dateTime && !!h.ids.updated.title, 'private header control is populated: ' + label);
    let finishRead, read;
    if (pendingKind.startsWith('data-') || pendingKind.startsWith('queue-')) {
      const path = pendingKind.startsWith('data-') ? '/data' : '/queue';
      const data = path === '/data' ? vm.runInContext('lastData', h.context) : queueExample;
      const pending = new Promise(resolve => { finishRead = resolve; });
      h.context.fetch = async target => target === path ? (pendingKind.endsWith('-json') ? {status:200,ok:true,json:() => pending} : pending) : {status:403,ok:false};
      read = vm.runInContext(path === '/data' ? 'refresh()' : 'loadQueue()', h.context);
      await settle();
      check(vm.runInContext(path === '/data' ? 'refreshing' : 'queueLoading', h.context), 'header proof holds a late read: ' + label);
      const resolveRead = finishRead;
      finishRead = () => resolveRead(pendingKind.endsWith('-json') ? data : {status:200,ok:true,json:async() => data});
    }
    const action = actionPending ? h.ids.freezeButton.click() : null;
    if (actionPending) check(h.ids.freezeButton.disabled && !h.ids.signOut.disabled, 'header proof holds a pending action');
    if (trigger === 'sign-out') h.ids.signOut.click();
    if (trigger === 'expiry') { clock.now += 7*24*60*60*1000; vm.runInContext('authenticated()', h.context); }
    if (trigger === 'cross-tab') { store.delete('nuntius.web.credential'); h.win.storage({storageArea:vm.runInContext('localStorage', h.context),key:'nuntius.web.credential'}); }
    if (trigger === 'server-rejection') {
      if (actionPending) { finishAction({status:403,ok:false}); await action; }
      else if (read) await vm.runInContext("action('unfreeze')", h.context);
      else { h.context.fetch = async () => ({status:403,ok:false}); await vm.runInContext('refresh()', h.context); }
    }
    const cleared = () => h.ids.freeze.textContent === 'Signed out' && h.ids.freeze.className === 'badge' && !['red','green','amber'].some(c => h.ids.freeze.classList.contains(c)) && h.ids.freeze.title === 'Signed out' && h.ids.freeze.getAttribute('aria-label') === 'Signed out' && headerCounts.every(id => h.ids[id].textContent === '—') && headerSnapshot(h) === neutralHeader;
    check(!vm.runInContext('authenticated()', h.context) && !store.has('nuntius.web.credential') && h.location.reloads === 1 && !h.ids.freeze.hidden && !h.ids.signInPanel.hidden && tabIDs.every(id => h.ids[id].hidden), 'header proof reaches signed-out view: ' + trigger);
    if (!cleared()) headerFailures.push(label + ' held reload');
    if (actionPending && trigger !== 'server-rejection') {
      finishAction({status:200,ok:true}); await action; await settle();
      if (!cleared()) headerFailures.push(label + ' late action');
    }
    if (read) { finishRead(); await read; await settle(); if (!cleared()) headerFailures.push(label + ' late read'); }
    h.timers[0]();
    if (!cleared()) headerFailures.push(label + ' timer');
  }
  check(headerFailures.length === 0, 'private header cleared immediately: ' + headerFailures.join(', '));
  for(const first of tabIDs) {
    const h=start('#'+first);await settle();
    check(h.ids.signInPanel && !h.ids.signInPanel.hidden && tabIDs.every(id=>h.ids[id].hidden), 'one sign-in panel replaces all signed-out content: '+first);
    check(h.ids.signInText.textContent.includes('nuntius web link') && h.ids.signInText.textContent.includes('iMessage') && h.ids.freeze.textContent==='Signed out' && h.ids.error.hidden,'clear sign-in instructions without Connecting: '+first);
    for(const next of tabIDs){h.ids['tab-'+next].click();await settle();check(!h.ids.signInPanel.hidden && tabIDs.every(id=>h.ids[id].hidden),'signed-out tab navigation retains only sign-in panel: '+next);}
    check(h.calls.length===0,'signed-out navigation makes no API requests');
  }
  check(input.page.includes('aria-controls="queue"') && input.page.includes('id="queueRows"'), 'approved Queue tab exists');
  const qc={status:200,current:queueExample},qclock={now:Date.parse('2026-10-10T22:33:00Z')};
  const q=start('#queue',new Map(),'?token=fixture-auth',false,qclock,200,null,null,undefined,qc);await settle();
  const visible=()=>q.ids.queueRows.children.filter(r=>!r.hidden);
  check(q.calls.filter(c=>c.path==='/queue').length===1 && q.ids.queueTabCount.textContent==='3','queue entry reads once and counts unfiltered snapshot');
  check(visible().map(r=>r.children[1].children[1].textContent).join(',')==='A,B,C','queue order and ASCII ID tie');
  const b=q.ids.queueRows.children[1],detail=b.children[1].children[2];detail.open=true;detail.children[0].focus();
  check(b.textContent.includes('<b>text only</b>') && b.querySelectorAll('b').length===0 && b.textContent.includes('EXTERNAL · outside this queue'),'text injection and external references');
  check(b.textContent.includes('Needs owner') && q.ids.queueRows.children[2].textContent.includes('Approve scope'),'owner holds independent of state');
  q.ids.queueKind.value='claude';q.ids.queueKind.listeners.change();q.ids.queueOwner.checked=true;q.ids.queueOwner.listeners.change();
  check(visible().length===2 && q.ids.queueTabCount.textContent==='3','multi-kind AND owner filter keeps total');
  q.ids.queueState.value='planned';q.ids.queueState.listeners.change();check(visible().length===1 && visible()[0].children[0].textContent==='8','state filter preserves original priority');
  q.ids.queueSearch.value='absent';q.ids.queueSearch.listeners.input();check(!q.ids.queueEmpty.hidden && q.ids.queueEmptyTitle.textContent==='No matching goals' && !q.ids.queueReset.hidden,'no matching goals with reset');
  q.ids.queueReset.click();check(visible().length===3,'clear filters');
  q.ids.queueSearch.value='First';q.ids.queueSearch.listeners.input();q.ids.queueSearch.focus();
  await vm.runInContext('loadQueue()',q.context);await settle();check(q.ids.queueSearch.value==='First' && q.doc.activeElement===q.ids.queueSearch && detail.open,'poll preserves filters focus and expanded details');
  q.ids.queueReset.click();detail.children[0].focus();await vm.runInContext('loadQueue()',q.context);check(q.doc.activeElement===detail.children[0],'focused details retain DOM identity');
  qc.status=503;qc.current={status:'invalid',goals:[]};await vm.runInContext('loadQueue()',q.context);
  check(q.ids.queueTabCount.textContent==='3' && !q.ids.queueBanner.hidden && q.ids.queueBanner.textContent.includes('Last known queue'),'failed refresh retains snapshot with banner');
  qc.status=200;qc.current=queueExample;await vm.runInContext('loadQueue()',q.context);check(q.ids.queueBanner.hidden,'fresh retry clears failure');
  q.ids.queueKind.value='claude';q.ids.queueKind.listeners.change();
  qc.current={...queueExample,goals:[queueExample.goals[1]]};await vm.runInContext('loadQueue()',q.context);
  check(q.ids.queueKind.value==='claude' && q.ids.queueRows.children.length===1 && q.ids.queueEmptyTitle.textContent==='No matching goals','disappearing kind preserves filter and removed focused goals disappear');
  qc.current=queueExample;q.ids.queueReset.click();await vm.runInContext('loadQueue()',q.context);
  qclock.now+=16*60*1000;await vm.runInContext('loadQueue()',q.context);check(q.ids.queueBanner.textContent.includes('snapshot is old'),'old publication warning');
  vm.runInContext("selectTab('agents')",q.context);const beforeQueue=q.calls.filter(c=>c.path==='/queue').length;await vm.runInContext('loadQueue();refresh()',q.context);
  check(q.calls.filter(c=>c.path==='/queue').length===beforeQueue && q.ids.lanes.children.length>0,'hidden queue has no poll and agents remain usable');
  q.ids['tab-agents'].listeners.keydown({key:'ArrowRight',preventDefault(){}});await settle();check(!q.ids.queue.hidden && q.doc.activeElement===q.ids['tab-queue'],'keyboard navigation reaches Queue');
  for(const state of ['not_configured','disabled','missing','invalid','unavailable','ok']){
    const control={status:['missing','invalid','unavailable'].includes(state)?503:200,current:{status:state,goals:[],updated:queueExample.updated}},h=start('#queue',new Map(),'?token=fixture-auth',false,undefined,200,null,null,undefined,control);await settle();
    check(!h.ids.queueEmpty.hidden && h.ids.queueRows.children.length===0,'isolated empty/error state '+state);
    if(state==='ok')check(h.ids.queueTabCount.textContent==='0' && h.ids.queueEmptyTitle.textContent==='No planned goals','published empty differs from missing');
    vm.runInContext("selectTab('agents')",h.context);check(h.ids.lanes.children.length>0,'queue error leaves Agents usable');
  }
  for(const ending of ['signout','storage','expiry'])for(const result of [200,403,503,'reject']){
    let finish,reject;const pendingQueue=new Promise((ok,no)=>{finish=ok;reject=no;});const clock={now:qclock.now},store=new Map(),control={pending:pendingQueue,status:200,current:queueExample};
    const h=start('#queue',store,'?token=fixture-auth',false,clock,200,null,null,undefined,control);await settle();
    const count=h.calls.length;await vm.runInContext('loadQueue()',h.context);check(h.calls.length===count && !h.ids.signOut.disabled,'queue dedupe and immediate sign-out availability');
    if(ending==='signout')h.ids.signOut.click();
    if(ending==='storage'){store.delete('nuntius.web.credential');h.win.storage({key:'nuntius.web.credential',storageArea:vm.runInContext('localStorage',h.context)});}
    if(ending==='expiry'){clock.now+=7*24*60*60*1000;vm.runInContext('authenticated()',h.context);}
    if(result==='reject')reject(new Error());else finish({ok:result===200,status:result,json:async()=>queueExample});await settle();
    await vm.runInContext('loadQueue()',h.context);check(h.ids.queueRows.children.length===0 && h.ids.queueTabCount.textContent==='—' && h.calls.length===count && !store.has('nuntius.web.credential'),'late queue '+result+' after '+ending);
  }
  const revokedQueue=start('#queue',new Map(),'?token=fixture-auth',false,undefined,200,null,null,undefined,{status:403,current:{}});await settle();check(revokedQueue.location.reloads===1,'queue revocation clears sign-in');
  const publicQueue=start('#queue');await settle();check(publicQueue.calls.length===0 && publicQueue.ids.queueRows.children.length===0,'public queue never requests private data');
  check(/queue-mobile-label/.test(input.css) && /max-width:760px/.test(input.css) && input.css.includes('min-height:44px'),'mobile labels and touch targets');
	check(input.page.includes('aria-controls="settings"') && input.page.includes('id="settingsForm"'), 'approved Settings tab and form exist');
  const settingsClock={now:1791658800000}, settingsStore=new Map();
  for(const code of ['locked','configuration_unavailable','invalid_configuration']) {
    const control={status:503,current:{code}},h=start('#settings',new Map(),'?token=fixture-auth',false,undefined,200,null,null,control);await settle();
    check(h.ids.settingsBanner.textContent.includes(code==='locked'?'busy, retrying':'nuntius config check') && (code==='locked' || h.ids.settingsBanner.textContent.includes(code==='configuration_unavailable'?'source':'invalid')), 'specific Settings read reason '+code);
    check(h.ids.settingsSave.disabled,'failed read cannot save '+code);
  }
  const retryControl={status:503,current:{code:'locked'}},retry=start('#settings',new Map(),'?token=fixture-auth',false,undefined,200,null,null,retryControl);await settle();
  const fireRetry=async h=>{const id=vm.runInContext('settingsRetry',h.context), timer=h.timeouts.get(id);check(timer?.delay===1000,'busy read schedules yielding retry');h.timeouts.delete(id);timer.fn();await settle();};
  for(let i=0;i<3;i++)await fireRetry(retry);
  check(retry.calls.filter(c=>c.path==='/settings').length===4 && vm.runInContext('settingsRetry',retry.context)===null && retry.ids.settingsBanner.textContent.includes('Reload current values to retry'), 'busy retry limit');
  retryControl.status=200;retryControl.current=exampleSettings;await retry.ids.settingsReload.click();await settle();
  check(retry.ids['setting-codex'].value==='2' && retry.ids.settingsBanner.hidden,'manual reload bypasses entry throttle after busy');
  retry.ids['setting-codex'].value='4';retry.ids['setting-codex'].listeners.input();retryControl.status=503;retryControl.current={code:'locked'};await vm.runInContext('loadSettings(true)',retry.context);
  retryControl.status=200;retryControl.current=exampleSettings;await fireRetry(retry);
  check(retry.ids['setting-codex'].value==='4' && !retry.ids.settingsSave.disabled,'busy retry keeps draft and restores confirmed baseline');
  for(const ending of ['signout','hidden','expiry']) {
    const clock={now:1791658800000},control={status:503,current:{code:'locked'}},h=start('#settings',new Map(),'?token=fixture-auth',false,clock,200,null,null,control);await settle();
    const timer=h.timeouts.get(vm.runInContext('settingsRetry',h.context)),count=h.calls.length;
    if(ending==='signout')h.ids.signOut.click();
    if(ending==='hidden')vm.runInContext("selectTab('agents')",h.context);
    if(ending==='expiry')clock.now+=7*24*60*60*1000;
    timer.fn();await settle();check(h.calls.length===count,'scheduled retry respects '+ending);
  }
  const settings=start('#settings',settingsStore,'?token=fixture-auth',false,settingsClock); await settle();
  check(!settings.ids.settings.hidden && settings.calls.filter(c=>c.path==='/settings').length===1 && settings.ids['setting-codex'].value==='2','settings hash loads authenticated current values once');
  check(settings.ids['setting-opencode'].disabled && !settings.ids['setting-codex'].disabled && settings.ids.settingsSave.disabled,'unconfigured pool disabled and unchanged save disabled');
  check(settings.ids.settingsModels.textContent.includes('read-only') && settings.ids.settingsMachine.textContent.includes('local only'),'read-only template and machine disposition');
  const edit=(h,value)=>{h.ids['setting-codex'].value=value;h.ids['setting-codex'].listeners.input();};
  edit(settings,'3'); settings.ids['setting-codex'].focus();
  check(!settings.ids.settingsSave.disabled && !settings.ids.settingsPending.hidden && settings.ids['pending-codex'].textContent.includes('2 → 3'),'valid draft has current/pending summary');
  await vm.runInContext('refresh()',settings.context); await settle();
  check(settings.ids['setting-codex'].value==='3' && settings.doc.activeElement===settings.ids['setting-codex'],'operational polling retains input and focus');
  vm.runInContext("selectTab('runs');selectTab('settings')",settings.context); await settle();
  check(settings.ids['setting-codex'].value==='3' && settings.calls.filter(c=>c.path==='/settings').length===1,'reentry preserves draft and throttles reads');
  for(const value of ['', '-1', '1.5', '9','1e0']){edit(settings,value);await vm.runInContext('saveSettings()',settings.context);check(settings.ids.settingsSave.disabled && settings.doc.activeElement===settings.ids['setting-codex'] && !settings.ids['error-codex'].hidden,'integer validation focuses inline error');}
  settings.ids.settingsCancel.click();
  check(settings.ids['setting-codex'].value==='2' && settings.ids.settingsSave.disabled && settings.ids['error-codex'].hidden,'Cancel restores baseline and clears errors');
  edit(settings,'3'); settings.ids['setting-notify'].checked=false; settings.ids['setting-notify'].listeners.change();
  settings.settingsControl.current={...exampleSettings,revision:'next-revision',pools:{...exampleSettings.pools,codex:{size:3,configured:true,editable:true}},notify_imessage_enabled:false,notify_effective:false};
  await settings.ids.settingsForm.listeners.submit({preventDefault(){}}); await settle();
  const savedSettings=settings.calls.find(c=>c.path==='/settings' && c.options.method==='POST'), batch=JSON.parse(savedSettings.options.body);
  check(batch.changes.pools.codex===3 && batch.changes.notify_imessage_enabled===false && Object.keys(batch.changes).length===2 && batch.expected_revision==='example-revision','Save submits allowlisted atomic batch');
  check(settings.ids.settingsSave.disabled && settings.ids.settingsBanner.textContent.startsWith('Saved.'),'success updates baseline');
  for(const result of [409,422,429,503,'reject']){
    const h=start('#settings',new Map(),'?token=fixture-auth');await settle();edit(h,'3');
    if(result==='reject')h.settingsControl.reject=true;else{h.settingsControl.status=result;h.settingsControl.current={code:result===409?'busy_agent':result===429?'rate_limited':'invalid_configuration',field:'pools.codex'};}
    await h.ids.settingsForm.listeners.submit({preventDefault(){}});await settle();
    check(h.ids['setting-codex'].value==='3' && !h.ids.settingsBanner.hidden,'failure preserves draft: '+result);
    if(result===422 || result===409)check(!h.ids['error-codex'].hidden && h.doc.activeElement===h.ids['setting-codex'],'server error remains inline after save');
  }
  settingsClock.now+=10000;edit(settings,'4');settings.settingsControl.current={...exampleSettings,revision:'external-revision'};
  await settings.ids.settingsReload.click();await settle();
  check(settings.ids['setting-codex'].value==='4' && settings.ids['current-codex'].textContent.includes('2') && settings.ids.settingsBanner.textContent.includes('draft is kept'),'Reload updates baseline without discarding draft');
  const legacy=start('#settings',new Map(),'?token=fixture-auth',false,undefined,200,null,null,{status:200,current:{...exampleSettings,format:'legacy',recipient_configured:false,pools:Object.fromEntries(Object.entries(exampleSettings.pools).map(([k,v])=>[k,{...v,editable:false}]))}});await settle();
  check(legacy.ids['setting-codex'].disabled && legacy.ids['setting-notify'].disabled && legacy.ids.settingsPoolHelp.textContent.includes('config convert'),'legacy and missing recipient local-only');
  for(const result of [200,403,503,'reject']){
    let finish,reject;const post=new Promise((ok,no)=>{finish=ok;reject=no;});const store=new Map();
    const h=start('#settings',store,'?token=fixture-auth');await settle();edit(h,'3');h.settingsControl.post=post;
    const save=h.ids.settingsForm.listeners.submit({preventDefault(){}});
    check(!h.ids.signOut.disabled && h.ids.settingsSave.disabled && h.ids.settingsCancel.disabled,'pending save keeps Sign out usable');
    h.ids.signOut.click();
    check(!store.has('nuntius.web.credential') && h.ids['setting-codex'].value==='' && h.ids.settingsModels.children.length===0 && h.ids.settingsReview.children.length===0,'sign-out immediately clears settings and drafts');
    const calls=h.calls.length;if(result==='reject')reject(new Error());else finish({ok:result===200,status:result,json:async()=>exampleSettings});await save;await settle();
    check(h.calls.length===calls && h.ids['setting-codex'].value==='' && h.ids.settingsBanner.hidden && !store.has('nuntius.web.credential'),'late settings response cannot resurrect private state: '+result);
  }
  const storageSettings=start('#settings',new Map(),'?token=fixture-auth');await settle();edit(storageSettings,'3');storageSettings.win.storage({key:'nuntius.web.credential',storageArea:vm.runInContext('localStorage',storageSettings.context)});
  check(storageSettings.ids['setting-codex'].value==='' && storageSettings.ids.settingsPending.hidden,'cross-tab settings draft clearing');
  const expiryClock={now:1791658800000}, expirySettings=start('#settings',new Map(),'?token=fixture-auth',false,expiryClock);await settle();edit(expirySettings,'3');expiryClock.now+=7*24*60*60*1000;await vm.runInContext('saveSettings()',expirySettings.context);
  check(expirySettings.ids['setting-codex'].value==='' && expirySettings.calls.filter(c=>c.path==='/settings').length===1,'expiry clears settings without a write');
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
  phoneCheck(expired.calls.length === 0 && !device.has('nuntius.web.credential') && !expired.ids.signInPanel.hidden && tabIDs.every(id=>expired.ids[id].hidden), 'exact expiry clears stored grant and requests sign-in');
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
    const pendingAction = h.ids.freezeButton.click();
    check(h.calls.length === 2 && h.calls[1].path === '/action' && h.ids.freezeButton.disabled, 'action is pending and locks action controls');
    h.ids.freezeButton.click();
    check(h.calls.length === 2, 'disabled-aware click suppresses a locked action');
    check(!h.ids.signOut.disabled, 'Sign out stays enabled during a pending action');
    h.ids.signOut.click();
    check(!h.ids.signInPanel.hidden && tabIDs.every(id=>h.ids[id].hidden), 'sign-out immediately hides private panels during pending action');
    check(!store.has('nuntius.web.credential') && h.location.reloads === 1 && !vm.runInContext('authenticated()', h.context), 'pending-action Sign out clears stored and in-memory credentials immediately and reloads the shell');
    const shell = start('#runs', store, '', false, clock); await settle();
    check(shell.calls.length === 0 && !rendered(shell, 'runs') && shell.ids.signOut.hidden, 'signed-out shell has no private data or authenticated requests');
    if (result === 'reject') reject(new Error());
    else finish({ok: result === 200, status: result, json: async () => input.data});
    check(await pendingAction === false, 'late signed-out action never reports success: ' + result);
    await settle();
    check(!store.has('nuntius.web.credential') && !vm.runInContext('authenticated()', h.context) && h.location.reloads === 1, 'late action cannot restore credentials or repeat sign-out: ' + result);
    check(h.ids.notice.hidden && h.ids.error.hidden && h.ids.runs.querySelectorAll('tbody').length === 0 && h.calls.length === 2, 'late action cannot restore notices, private data or refresh requests: ' + result);
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
  check(lane.ids.laneTail.textContent.endsWith('Report saved.'), 'lane tail chronological with latest line at bottom');
  check(lane.ids.laneNotes.textContent.includes('codex-2 note') && !lane.ids.laneNotes.textContent.includes('claude-1 note'), 'lane owner notes isolated');
  check(lane.ids.laneSignals.textContent.includes('codex-2.REVIEW-RELEASE.ended'), 'lane signals visible');
  vm.runInContext("lastData = JSON.parse(JSON.stringify(lastData)); lastData.lanes.push({lane:'codex-3',kind:'codex',model:'runtime',effort:'default',state:'idle',eta:'-',activity:'-'}); render(lastData)",lane.context);
  check(lane.ids.agentTabs.children.length === input.data.lanes.length + 2, 'runtime lane added');
  vm.runInContext("selectTab('agents/codex-3')",lane.context);
  check(lane.location.hash === '#agents/codex-3' && lane.ids.lanes.textContent.includes('runtime'), 'runtime lane routing and model');
  lane.location.hash = '#agents/codex-2'; lane.win.hashchange();
  check(lane.ids.laneHistory.textContent.includes('REVIEW-RELEASE'), 'back/forward hash routing');
  vm.runInContext("lastData.tails['codex-2'].push('Newest action'); render(lastData)",lane.context);
  check(lane.ids.laneTail.textContent.endsWith('Newest action'), 'live lane tail follows');
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
  check(unsigned.calls.length === 0 && !unsigned.ids.signInPanel.hidden && unsigned.ids.error.hidden, 'empty tab storage shows sign-in panel without data requests');
  await vm.runInContext("action('unfreeze')", unsigned.context);
  check(unsigned.calls.length === 0, 'unsigned action does not send API requests');
  const blocked = start('#runs', new Map(), '?token=fixture-auth', true); await settle();
  check(rendered(blocked, 'runs') && !blocked.location.search, 'storage failure permits current sign-in and clears URL');
  const noStorage = start('#runs', new Map(), '', true); await settle();
  check(noStorage.calls.length === 0 && !noStorage.ids.signInPanel.hidden, 'storage failure without bootstrap remains signed out');
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
		Kind              string            `json:"kind"`
		Model             string            `json:"model"`
		Effort            string            `json:"effort"`
		Relation          string            `json:"relation"`
		ChainID           string            `json:"chain_id"`
		ChainDepth        int               `json:"chain_depth"`
		RunID             string            `json:"run_id"`
		Lane              string            `json:"lane"`
		GoalID            string            `json:"goal_id"`
		Started           time.Time         `json:"started"`
		Ended             *time.Time        `json:"ended"`
		State             string            `json:"state"`
		DisplayState      string            `json:"display_state"`
		Exit              *int              `json:"exit"`
		Goal              string            `json:"goal"`
		Report            string            `json:"report"`
		Sections          map[string]string `json:"sections"`
		Overview          reportOverview    `json:"overview"`
		ReportAssociation string            `json:"report_association"`
		ResumeCount       int               `json:"resume_count"`
		Badge             string            `json:"badge"`
		ParentRunID       string            `json:"parent_run_id"`
		ContinueRunID     string            `json:"continue_run_id"`
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
