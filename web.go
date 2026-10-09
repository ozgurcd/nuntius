package nuntius

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Web struct {
	manager        *Manager
	address, token string
}

func NewWeb(m *Manager, address string) (*Web, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil || host != "127.0.0.1" {
		return nil, errors.New("web must bind to 127.0.0.1 only")
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 0 || n > 65535 {
		return nil, errors.New("invalid web port")
	}
	b := make([]byte, 32)
	if _, e = rand.Read(b); e != nil {
		return nil, e
	}
	return &Web{m, address, hex.EncodeToString(b)}, nil
}

func (w *Web) ServeHTTP(out http.ResponseWriter, r *http.Request) {
	out.Header().Set("Cache-Control", "no-store")
	out.Header().Set("Referrer-Policy", "no-referrer")
	out.Header().Set("X-Content-Type-Options", "nosniff")
	out.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	host, port, e := net.SplitHostPort(r.Host)
	_, wantPort, _ := net.SplitHostPort(w.address)
	if e != nil || (host != "127.0.0.1" && host != "localhost") || port != wantPort {
		http.Error(out, "forbidden", http.StatusForbidden)
		return
	}
	auth := r.Header.Get("X-Nuntius-Token")
	if auth == "" && r.Method == http.MethodGet {
		auth = r.URL.Query().Get("token")
	}
	if subtle.ConstantTimeCompare([]byte(auth), []byte(w.token)) != 1 {
		http.Error(out, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost && r.Header.Get("Origin") != "http://"+r.Host {
		http.Error(out, "forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/action" {
		if r.Method != http.MethodPost {
			out.Header().Set("Allow", "POST")
			http.Error(out, "POST required", http.StatusMethodNotAllowed)
			return
		}
		w.action(out, r)
		return
	}
	if r.Method != http.MethodGet {
		out.Header().Set("Allow", "GET")
		http.Error(out, "GET required", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/":
		out.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(out, dashboardHTML)
	case "/data":
		v, e := w.data()
		if e != nil {
			http.Error(out, "dashboard data unavailable", http.StatusInternalServerError)
			return
		}
		out.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(out).Encode(safeValue(v))
	default:
		http.NotFound(out, r)
	}
}

func (w *Web) action(out http.ResponseWriter, r *http.Request) {
	b, e := io.ReadAll(http.MaxBytesReader(out, r.Body, 65536))
	var a struct {
		Action string `json:"action"`
		Lane   string `json:"lane"`
		Text   string `json:"text"`
		Stop   bool   `json:"stop"`
	}
	if e != nil || strictJSON(b, &a) != nil {
		http.Error(out, "invalid action", http.StatusBadRequest)
		return
	}
	switch a.Action {
	case "freeze":
		e = w.manager.Freeze(a.Text)
	case "unfreeze":
		e = w.manager.Unfreeze()
	case "cancel":
		e = w.manager.Cancel(a.Lane)
	case "message":
		e = w.manager.SendMessage(a.Lane, a.Text, a.Stop)
	case "note":
		e = w.manager.AddOwnerNote(a.Text)
	default:
		e = errors.New("unknown action")
	}
	if e != nil {
		http.Error(out, bounded(Redact(e.Error()), 4096), http.StatusBadRequest)
		return
	}
	out.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(out, `{"ok":true}`)
}

func readDisplay(path string, limit int) string {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return ""
	}
	defer f.Close()
	fi, e := f.Stat()
	if e != nil || !fi.Mode().IsRegular() {
		return ""
	}
	b, e := io.ReadAll(io.LimitReader(f, int64(limit+4096)))
	if e != nil {
		return ""
	}
	return bounded(Redact(string(b)), limit)
}

func reportSections(report string) map[string]string {
	sections := map[string]string{"QUESTIONS": "", "DECISIONS": ""}
	current := ""
	for _, line := range strings.Split(report, "\n") {
		head := strings.TrimSpace(strings.TrimLeft(line, "# "))
		label, inline, colon := strings.Cut(head, ":")
		label = strings.TrimSpace(label)
		if label == "QUESTIONS" || label == "DECISIONS" {
			current = label
			if colon && strings.TrimSpace(inline) != "" {
				sections[current] += strings.TrimSpace(inline) + "\n"
			}
			continue
		}
		if (colon || strings.HasPrefix(strings.TrimSpace(line), "#")) && label == strings.ToUpper(label) {
			current = ""
		}
		if current != "" {
			sections[current] += line + "\n"
		}
	}
	return sections
}

func (w *Web) data() (any, error) {
	m := w.manager
	status, e := m.Status()
	if e != nil {
		return nil, e
	}
	m.mu.Lock()
	runs, e := m.history()
	config := m.config
	m.mu.Unlock()
	if e != nil {
		return nil, e
	}
	if len(runs) > 50 {
		runs = runs[:50]
	}
	views := []any{}
	for _, r := range runs {
		report, _ := m.Report(r.GoalID)
		views = append(views, map[string]any{"run_id": r.ID, "lane": r.Lane, "goal_id": r.GoalID, "started": r.Started, "ended": r.Ended, "state": r.State, "exit": r.Exit, "goal": readDisplay(filepath.Join(m.store, "runs", r.ID, "goal.txt"), 12000), "report": report, "sections": reportSections(report)})
	}
	tails := map[string][]string{}
	for _, s := range status {
		tails[s.Lane], _ = m.Tail(s.Lane, 50)
	}
	notes, e := m.OwnerNotes(time.Time{})
	if e != nil {
		return nil, e
	}
	oversight := config.OversightPath
	if oversight == "" {
		oversight = filepath.Join(filepath.Dir(config.ReportsDir), "OVERSIGHT.md")
	}
	audit, _ := readWindow(filepath.Join(m.store, "audit.jsonl"), true)
	frozen, e := m.freezeState()
	if e != nil {
		return nil, e
	}
	return map[string]any{"freeze": frozen, "lanes": status, "tails": tails, "history": views, "notes": notes, "audit": bounded(Redact(string(audit)), 64000), "oversight": readDisplay(oversight, 64000)}, nil
}

func (w *Web) Run(ctx context.Context, out io.Writer) error {
	listener, e := net.Listen("tcp4", w.address)
	if e != nil {
		return errors.New("cannot listen on local web address")
	}
	w.address = listener.Addr().String()
	server := &http.Server{Handler: w, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	fmt.Fprintf(out, "http://%s/?token=%s\n", w.address, w.token)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		case <-done:
		}
	}()
	e = server.Serve(listener)
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}

const dashboardHTML = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Nuntius oversight</title>
<style>body{font:16px system-ui;max-width:1100px;margin:2rem auto;padding:0 1rem;background:#111827;color:#e5e7eb}button,textarea,select{font:inherit;padding:.6rem;margin:.25rem;border-radius:6px}button{cursor:pointer}textarea{width:90%;height:5rem}article,details{background:#1f2937;padding:1rem;margin:1rem 0;border-radius:10px}pre{white-space:pre-wrap;overflow-wrap:anywhere}.questions{border-left:4px solid #fbbf24;padding:1rem}.decisions{border-left:4px solid #60a5fa;padding:1rem}.muted{color:#9ca3af}h1{font-size:1.8rem}</style>
<h1>Nuntius oversight</h1><p id="freeze"></p><button id="freezeButton">FREEZE ALL</button><button id="unfreezeButton">UNFREEZE</button><p id="error" role="alert"></p>
<h2>Agents</h2><div id="lanes"></div><h2>Message an agent</h2><select id="lane"></select><br><textarea id="message" aria-label="Agent message"></textarea><br><button id="send">Send / queue after run</button><button id="stopSend">Stop and send</button>
<h2>Owner note to judge</h2><textarea id="note" aria-label="Owner note"></textarea><br><button id="saveNote">Save note</button><div id="notes"></div>
<h2>Owner digest</h2><article id="oversight"></article><h2>Run history</h2><div id="history"></div><details><summary>Audit log</summary><pre id="audit"></pre></details>
<script>
const token=new URLSearchParams(location.search).get('token')||'';history.replaceState(null,'','/');
const el=id=>document.getElementById(id),pre=(parent,text)=>{let p=document.createElement('pre');p.textContent=text;parent.append(p);return p};
async function action(action,lane='',text='',stop=false){try{let r=await fetch('/action',{method:'POST',headers:{'X-Nuntius-Token':token,'Content-Type':'application/json'},body:JSON.stringify({action,lane,text,stop})});if(!r.ok)throw Error(await r.text());el('error').textContent='';await refresh()}catch(e){el('error').textContent=e.message}}
el('freezeButton').onclick=()=>action('freeze','','Owner dashboard freeze');el('unfreezeButton').onclick=()=>action('unfreeze');el('send').onclick=()=>action('message',el('lane').value,el('message').value);el('stopSend').onclick=()=>action('message',el('lane').value,el('message').value,true);el('saveNote').onclick=()=>action('note','',el('note').value);
async function refresh(){try{let r=await fetch('/data',{headers:{'X-Nuntius-Token':token}});if(!r.ok)throw Error('Dashboard unavailable');let d=await r.json();el('freeze').textContent=d.freeze?'FROZEN since '+d.freeze.since+' — '+d.freeze.reason:'Dispatch enabled';el('lanes').replaceChildren();let chosen=el('lane').value;el('lane').replaceChildren();for(let s of d.lanes){let a=document.createElement('article');pre(a,s.lane+' · '+s.state+' · '+(s.goal_id||'')+' · elapsed '+s.elapsed_seconds+'s · last event '+(s.last_event_age_seconds??'—')+'s ago');pre(a,(d.tails[s.lane]||[]).join('\n'));if(s.state==='running'){let b=document.createElement('button');b.textContent='Cancel '+s.lane;b.onclick=()=>action('cancel',s.lane);a.append(b)}el('lanes').append(a);let o=document.createElement('option');o.value=s.lane;o.textContent=s.lane;el('lane').append(o)}if(chosen)el('lane').value=chosen;el('notes').replaceChildren();for(let n of d.notes)pre(el('notes'),n.time+' · '+(n.read?'read':'unread')+'\n'+n.text);el('oversight').replaceChildren();for(let line of d.oversight.split('\n')){let match=line.match(/^(#{1,3}) (.*)$/),p=document.createElement(match?'h'+match[1].length:'p');p.textContent=match?match[2]:line;el('oversight').append(p)}el('history').replaceChildren();for(let run of d.history){let a=document.createElement('details'),s=document.createElement('summary');s.textContent=run.lane+' · '+run.goal_id+' · '+run.state+' · '+run.started;a.append(s);for(let name of ['QUESTIONS','DECISIONS']){let p=pre(a,name+'\n'+run.sections[name]);p.className=name.toLowerCase()}pre(a,'Goal\n'+run.goal);pre(a,'Report\n'+run.report);el('history').append(a)}el('audit').textContent=d.audit}catch(e){el('error').textContent=e.message}}refresh();setInterval(refresh,2000);
</script></html>`
