package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The wire shape lets these regressions run against the unmodified U01 parent.
type overviewFixture struct {
	Blocks     map[string][]string `json:"blocks"`
	OwnerNeed  string              `json:"owner_need"`
	Incomplete bool                `json:"incomplete"`
	Conflict   bool                `json:"status_conflict"`
	Attention  bool                `json:"attention"`
	Warnings   []string            `json:"warnings"`
}

func overviewRun(t *testing.T, w *Web) (string, overviewFixture) {
	t.Helper()
	d, err := w.data()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		History []struct {
			Report   string
			Overview overviewFixture
		}
	}
	if json.Unmarshal(b, &wire) != nil || len(wire.History) == 0 {
		t.Fatal("missing fixture history")
	}
	return wire.History[0].Report, wire.History[0].Overview
}

func TestReportOverviewShapes(t *testing.T) {
	cases := []struct {
		name, report, need              string
		incomplete, conflict, attention bool
		questionBlocks                  int
	}{
		{"inline", "PLAIN SUMMARY: exact summary\nSTATUS: complete\nITEMS: not status\nQUESTIONS: (a) ship\n(b) hold\nDECISIONS: routine\nIDS: local\nHEADS: dirty\nOPEN: wait", "decision", false, false, true, 1},
		{"multiline", "PLAIN SUMMARY:\nexact summary\nSTATUS:\ncomplete\nQUESTIONS:\n(a) ship\n(b) hold\nIDS:\nlocal\nHEADS:\ndirty\nOPEN:\nwait", "decision", false, false, true, 1},
		{"outer_fence_crlf", "```text\r\nPLAIN SUMMARY: exact summary\r\nSTATUS: complete\r\nQUESTIONS: none\r\n```", "none", false, false, false, 1},
		{"legacy", "## PLAIN SUMMARY\nexact summary\n## STATUS\ncomplete\n## QUESTIONS\nnone recorded.\n## OPEN\nnone", "none", false, false, false, 1},
		{"standalone", "PLAIN SUMMARY\nexact summary\nSTATUS\ncomplete\nITEMS\nnot status\nQUESTIONS\nno open questions", "none", false, false, false, 1},
		{"nested_fence", "PLAIN SUMMARY: exact summary\nSTATUS: complete\nQUESTIONS: none\nITEMS:\n```go\nQUESTIONS: fake\nSTATUS: STOP\n```\nOPEN: none", "none", false, false, false, 1},
		{"duplicate", "PLAIN SUMMARY: exact summary\nSTATUS: complete\nQUESTIONS: (a) ship\nQUESTIONS: (b) hold", "unknown", true, false, true, 2},
		{"conflicting_status", "PLAIN SUMMARY: exact summary\nSTATUS: complete\nSTATUS: partial\nQUESTIONS: none", "unknown", true, true, true, 1},
		{"missing", "PLAIN SUMMARY: exact summary\nSTATUS: complete", "unknown", false, false, true, 0},
		{"truncated", "PLAIN SUMMARY: exact summary\nSTATUS: complete\nQUESTIONS: none\n[truncated]", "unknown", true, false, true, 1},
		{"redacted", "PLAIN SUMMARY: exact summary [REDACTED]\nSTATUS: complete\nQUESTIONS: none", "unknown", true, false, true, 1},
		{"xss", "PLAIN SUMMARY: exact summary <img src=x onerror=alert(1)>\nSTATUS: complete\nQUESTIONS: <script>alert(1)</script>", "decision", false, false, true, 1},
		{"partial_none", "PLAIN SUMMARY: exact summary\nSTATUS: partial with parked work\nQUESTIONS: none", "none", false, false, true, 1},
		{"none_is_exact", "PLAIN SUMMARY: exact summary\nSTATUS: complete\nQUESTIONS: none except choose (a) or (b)", "decision", false, false, true, 1},
		{"unknown_format", "<html>plain summary: guessed</html>", "unknown", false, false, true, 0},
		{"uppercase_prose", "PLAIN SUMMARY: exact summary\nSTATUS: complete\nQUESTIONS:\nChoose HTTP:\n(a) one\n(b) two\nOPEN: none", "decision", false, false, true, 1},
		{"mismatch", "PLAIN SUMMARY: exact summary. Owner must approve launch.\nSTATUS: complete\nQUESTIONS: none\nOPEN: Await owner decision", "none", false, false, true, 1},
	}
	m, c, _ := manager(t)
	_, err := m.Dispatch("codex", "OVERVIEW", "hello")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(c.ReportsDir, "OVERVIEW.txt"), []byte(tc.report), 0600); err != nil {
				t.Fatal(err)
			}
			raw, p := overviewRun(t, w)
			if p.OwnerNeed != tc.need || p.Incomplete != tc.incomplete || p.Conflict != tc.conflict || p.Attention != tc.attention {
				t.Fatal("conservative interpretation missing")
			}
			if len(p.Blocks["QUESTIONS"]) != tc.questionBlocks {
				t.Fatal("source blocks lost")
			}
			if tc.name != "unknown_format" && !strings.Contains(strings.Join(p.Blocks["PLAIN SUMMARY"], ""), "exact summary") {
				t.Fatal("verbatim summary missing")
			}
			if strings.Contains(strings.Join(p.Blocks["STATUS"], ""), "not status") {
				t.Fatal("contract boundary leaked")
			}
			if raw != bounded(tc.report, 65536) {
				t.Fatal("raw redacted report changed")
			}
			if (tc.name == "duplicate" || tc.name == "mismatch") && len(p.Warnings) == 0 {
				t.Fatal("ambiguity notice missing")
			}
		})
	}
}

func TestReportOverviewCache(t *testing.T) {
	m, c, _ := manager(t)
	_, err := m.Dispatch("codex", "CACHE", "hello")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.ReportsDir, "CACHE.txt")
	write := func(s string) {
		t.Helper()
		if os.WriteFile(path, []byte(s), 0600) != nil {
			t.Fatal("fixture write")
		}
	}
	write("PLAIN SUMMARY: first\nSTATUS: complete\nQUESTIONS: none")
	fi, _ := os.Stat(path)
	_, a := overviewRun(t, w)
	cache := reflect.ValueOf(w).Elem().FieldByName("reports")
	if !cache.IsValid() {
		t.Fatal("bounded projection cache absent")
	}
	parses := func() uint64 { return cache.FieldByName("parses").Uint() }
	before := parses()
	for i := 0; i < 10; i++ {
		overviewRun(t, w)
		out := httptest.NewRecorder()
		w.menubarHandler().ServeHTTP(out, httptest.NewRequest("GET", "/status", nil))
		if out.Code != 200 {
			t.Fatal("offline menu status")
		}
	}
	if before != parses() {
		t.Fatal("unchanged poll reparsed report")
	}
	write("PLAIN SUMMARY: other\nSTATUS: complete\nQUESTIONS: none")
	if os.Chtimes(path, fi.ModTime(), fi.ModTime()) != nil {
		t.Fatal("fixture timestamp")
	}
	_, b := overviewRun(t, w)
	if strings.Join(a.Blocks["PLAIN SUMMARY"], "") == strings.Join(b.Blocks["PLAIN SUMMARY"], "") || parses() != before+1 {
		t.Fatal("same length timestamp rewrite reused stale projection")
	}
	for i := 0; i < 140; i++ {
		write("PLAIN SUMMARY: " + strings.Repeat("x", i) + "\nQUESTIONS: none")
		overviewRun(t, w)
	}
	if cache.FieldByName("entries").Len() > 128 {
		t.Fatal("unbounded cache")
	}
}

func TestReportOverviewAttemptAndPrivacy(t *testing.T) {
	m, c, _ := manager(t)
	_, err := m.Dispatch("codex", "BOUND", "hello")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	path := filepath.Join(c.ReportsDir, "BOUND.txt")
	report := "PLAIN SUMMARY: safe\nSTATUS: complete\nQUESTIONS: none\n-----BEGIN PRIVATE KEY-----\nprivate-fixture\n-----END PRIVATE KEY-----"
	if os.WriteFile(path, []byte(report), 0600) != nil {
		t.Fatal("fixture write")
	}
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	raw, p := overviewRun(t, w)
	if strings.Contains(raw, "private-fixture") || p.OwnerNeed != "unknown" || !p.Incomplete {
		t.Fatal("overview privacy interpretation")
	}
	_, err = m.Dispatch("codex", "BOUND", "hello")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	raw, p = overviewRun(t, w)
	if raw != "" || p.OwnerNeed != "unknown" {
		t.Fatal("old report certified new attempt")
	}
}

func TestReportOverviewRuntime(t *testing.T) {
	for _, name := range []string{"overview", "none_unknown", "partial_exit", "duplicates", "incomplete", "xss", "repeated_resume", "focus", "signout", "tail_follow", "tail_pause", "tail_cadence", "tail_redaction", "tail_lane", "tail_window"} {
		t.Run(name, func(t *testing.T) {
			assets := map[string]any{"test": name}
			for key, path := range map[string]string{"script": "app.js", "page": "index.html", "css": "app.css"} {
				b, e := dashboardAssets.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				assets[key] = string(b)
			}
			b, e := os.ReadFile("testdata/dashboard-data.json")
			if e != nil {
				t.Fatal(e)
			}
			assets["data"] = json.RawMessage(b)
			in, e := json.Marshal(assets)
			if e != nil {
				t.Fatal(e)
			}
			prefix := dashboardRuntimeTest[:strings.Index(dashboardRuntimeTest, "(async () => {")]
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "node", "-e", prefix+overviewRuntimeTest)
			cmd.Stdin = bytes.NewReader(in)
			var diag bytes.Buffer
			cmd.Stderr = &diag
			if e = cmd.Run(); e != nil {
				t.Fatalf("U01 predicate: %v (%s)", e, diag.String())
			}
		})
	}
}

const overviewRuntimeTest = `
(async () => {
 const h=start('#runs',new Map(),'?token=fixture-auth'); await settle();
 const run={...input.data.history[0],report:'PLAIN SUMMARY: exact summary\nSTATUS: partial\nQUESTIONS: (a) ship\n(b) hold',
  overview:{blocks:{'PLAIN SUMMARY':['exact summary\n'],STATUS:['partial\n'],QUESTIONS:['(a) ship\n(b) hold\n'],IDS:['local commit\n'],HEADS:['ahead; no push\n'],OPEN:['await timing\n']},owner_need:'decision',attention:true,incomplete:false,status_conflict:false,warnings:[]}};
 const put=r=>{h.context.fixture=r;vm.runInContext('lastData.history=[fixture];render(lastData)',h.context);};
 put(run);
 const view=vm.runInContext('runs.values().next().value',h.context); view.toggle.click();
 const text=()=>view.detail.textContent;
 if(input.test==='overview'){
  for(const s of ['What happened','Report says','exact summary','partial','Does it need me','(a) ship','(b) hold','Commits and pushes','Reported, not independently checked','local commit','ahead; no push','What is next','await timing','Read full report','Read original goal','Ended','Received'])check(text().includes(s),'overview visible with provenance: '+s);
  check(h.ids.historyBound.textContent.includes('1 matching attempts from the latest 1 received'),'honest received bound');
  check(!input.css.includes('.report-text{max-height:480px'),'page scroll for raw reports');
 }
 if(input.test==='partial_exit')check(text().includes('Process ended normally')&&text().includes('report says partial')&&text().includes('Needs your decision'),'exit success is separate from partial report');
 if(input.test==='none_unknown'){
  run.overview.owner_need='none';run.overview.blocks.QUESTIONS=['none\n'];put(run);view.detail.hidden=true;view.toggle.click();check(text().includes('No owner decision reported'),'explicit none');
  delete run.overview;run.report='unstructured';run.sections={};put(run);view.detail.hidden=true;view.toggle.click();check(text().includes('Owner need unknown')&&!text().includes('None recorded.'),'missing is unknown');
 }
 if(input.test==='duplicates'||input.test==='incomplete'){
  run.overview.incomplete=true;run.overview.owner_need='unknown';run.overview.status_conflict=true;run.overview.warnings=['Repeated QUESTIONS section'];run.overview.blocks.QUESTIONS=['(a) ship\n','(b) hold\n'];put(run);view.detail.hidden=true;view.toggle.click();check(text().includes('Incomplete')&&text().includes('Conflicting report status')&&text().includes('Repeated QUESTIONS')&&text().includes('(a) ship')&&text().includes('(b) hold'),'incomplete duplicate evidence remains visible');
 }
 if(input.test==='xss'){
  run.overview.blocks['PLAIN SUMMARY']=['<img src=x onerror=alert(1)>'];run.overview.blocks.QUESTIONS=['<script>alert(1)</script>'];put(run);view.detail.hidden=true;view.toggle.click();check(text().includes('<img')&&view.detail.querySelectorAll('img').length===0&&view.detail.querySelectorAll('script').length===0,'XSS shaped summary and questions are text');
 }
 if(input.test==='repeated_resume'){
  h.context.fixture=[run,{...run,run_id:'resumed',relation:'resumed after interruption',chain_depth:1}];vm.runInContext('lastData.history=fixture;render(lastData)',h.context);
  for(const v of vm.runInContext('[...runs.values()]',h.context)){v.detail.hidden=true;v.toggle.click();check(v.detail.textContent.includes('Needs your decision')&&v.detail.textContent.includes('(b) hold'),'repeated resumed question preserved per attempt');}
  check(h.ids.questionsTabCount.title.includes('received attempts'),'attention count population');
 }
 if(input.test==='focus'){
  check(view.rawReport&&view.rawGoal,'raw disclosures exist');view.rawReport.open=true;view.rawGoal.open=true;view.rawReport.firstElementChild.focus();const summary=view.rawReport.firstElementChild;
  put({...run,overview:{...run.overview,blocks:{...run.overview.blocks,'PLAIN SUMMARY':['new summary']}}});check(view.rawReport.open&&view.rawGoal.open&&h.doc.activeElement===summary&&text().includes('exact summary'),'focus expansion and reading content survive poll');
 }
 if(input.test==='signout'){
  vm.runInContext("el('message').value='private draft';el('note').value='private note'",h.context);
  let finish;const wait=new Promise(r=>finish=r);h.context.fetch=async()=>wait;const late=vm.runInContext('refresh()',h.context);await settle();h.ids.signOut.click();
  check(view.detail.textContent===''&&vm.runInContext('lastData===undefined && runs.size===0 && cards.size===0',h.context)&&h.ids.message.value===''&&h.ids.note.value==='','signout synchronously drops overview cache and drafts');
  finish({status:200,ok:true,json:async()=>input.data});await late;check(h.ids.runs.querySelectorAll('tbody').length===0,'late reply cannot restore overview');
 }
 if(input.test.startsWith('tail_')){
  vm.runInContext("selectTab('agents')",h.context);const card=vm.runInContext("cards.get('claude-1')",h.context);card.tail.open=true;card.tail.listeners.toggle?.();
  check(card.tailJump&&card.tailAge,'live tail controls exist');
  card.tailText.scrollHeight=1000;card.tailText.clientHeight=100;card.tailText.scrollTop=900;
  if(input.test==='tail_pause'){card.tailText.scrollTop=100;card.tailText.listeners.scroll();}
  const focused=card.tail.firstElementChild;focused.focus();
  vm.runInContext("lastData.tails['claude-1'].push('new bounded line');render(lastData)",h.context);
  check(card.tailText.textContent.endsWith('new bounded line'),'open focused tail follows on existing data refresh');
  if(input.test==='tail_follow')check(card.tailText.scrollTop===1000&&h.doc.activeElement===focused&&card.tailAge.textContent.includes('last line 8s ago'),'auto scroll and last line age');
  if(input.test==='tail_pause'){check(card.tailText.scrollTop===100&&card.tailJump.textContent==='1 new lines - jump to latest'&&!card.tailJump.hidden,'scroll up pauses with new line count');card.tailJump.click();check(card.tailText.scrollTop===1000&&card.tailJump.hidden,'jump resumes follow');}
  if(input.test==='tail_cadence'){check(h.timers.length===3,'no extra timer');const before=h.calls.length;h.timers[1]();await settle();check(h.calls.length===before+1&&h.calls.at(-1).path==='/data','only existing operational poll');}
  if(input.test==='tail_redaction'){vm.runInContext("lastData.tails['claude-1'].push('[REDACTED]');render(lastData)",h.context);check(card.tailText.textContent.endsWith('[REDACTED]')&&!card.tailText.querySelector('a'),'redacted bounded lines remain plain text');}
  if(input.test==='tail_lane'){
   vm.runInContext("selectTab('agents/claude-1')",h.context);h.ids.laneTail.scrollHeight=1000;h.ids.laneTail.clientHeight=100;h.ids.laneTail.scrollTop=100;h.ids.laneTail.listeners.scroll();h.ids.laneTail.focus();
   vm.runInContext("lastData.tails['claude-1'].push('lane follow');render(lastData)",h.context);
   const v=vm.runInContext('laneTailView',h.context);check(h.ids.laneTail.textContent.endsWith('lane follow')&&h.ids.laneTail.scrollTop===100&&v.tailJump.textContent==='1 new lines - jump to latest','selected lane shares live follow pause and counter');v.tailJump.click();check(h.ids.followTail.checked&&h.ids.laneTail.scrollTop===1000,'selected lane jump');
  }
  if(input.test==='tail_window'){
   const first=card.tailText.firstElementChild;
   vm.runInContext("lastData.tails['claude-1'].push('overlap');render(lastData)",h.context);check(first===card.tailText.firstElementChild,'append retains old line nodes');
   vm.runInContext("lastData.tails['claude-1']=['overlap','window last'];render(lastData)",h.context);check(card.tailText.children.length===2&&card.tailText.textContent==='overlap\nwindow last','bounded overlap evicts old nodes');
   vm.runInContext("lastData.tails['claude-1']=['rotated'];render(lastData)",h.context);check(card.tailText.textContent==='rotated'&&card.tailNotice.textContent.includes('older lines are unavailable'),'rotation labels lost context');
   vm.runInContext("lastData.lanes[0].run_id='nextattempt';lastData.tails['claude-1']=['new attempt'];render(lastData)",h.context);check(card.tailText.textContent==='new attempt'&&!card.tailText.textContent.includes('rotated'),'attempt switch clears previous tail');
  }
 }
})().catch(()=>{process.stderr.write(predicate);process.exitCode=1;});
`
