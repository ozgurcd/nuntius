package nuntius

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// laneCLIRequest carries only selectors and a reviewed content identity, never
// message text or a session value supplied by a caller.
type laneCLIRequest struct {
	operation, lane, run, digest string
	revision                     uint64
	after, machine               bool
}

func parseLaneCLI(args []string) (laneCLIRequest, bool) {
	r := laneCLIRequest{}
	if len(args) == 0 {
		return r, false
	}
	r.operation = args[0]
	switch r.operation {
	case "list":
		r.machine = len(args) == 2 && args[1] == "--json"
		return r, len(args) == 1 || r.machine
	case "pause", "resume":
		if len(args) < 2 || !namePattern.MatchString(args[1]) {
			return r, false
		}
		r.lane = args[1]
		r.after = r.operation == "pause" && len(args) == 3 && args[2] == "--after-current"
		return r, len(args) == 2 || r.after
	case "held":
		if len(args) < 4 || !namePattern.MatchString(args[2]) || !runPattern.MatchString(args[3]) {
			return r, false
		}
		r.operation = "held-" + args[1]
		r.lane = args[2]
		r.run = args[3]
		if args[1] == "review" {
			return r, len(args) == 4
		}
		if args[1] != "send" && args[1] != "discard" {
			return r, false
		}
		if len(args) != 8 {
			return r, false
		}
		seen := map[string]bool{}
		for i := 4; i < 8; i += 2 {
			if seen[args[i]] {
				return r, false
			}
			seen[args[i]] = true
			switch args[i] {
			case "--revision":
				var err error
				r.revision, err = strconv.ParseUint(args[i+1], 10, 64)
				if err != nil || r.revision == 0 || r.revision > laneControlMaxRevision || strconv.FormatUint(r.revision, 10) != args[i+1] {
					return r, false
				}
			case "--digest":
				r.digest = args[i+1]
				if len(r.digest) != 64 || strings.Trim(r.digest, "0123456789abcdef") != "" {
					return r, false
				}
			default:
				return r, false
			}
		}
		return r, seen["--revision"] && seen["--digest"]
	}
	return r, false
}

func (m *Manager) laneCommand(r laneCLIRequest, out, errOut io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(errOut, laneCLIError(err)); return 1 }
	if r.operation == "list" {
		s, err := m.laneControls()
		code := 0
		if err != nil {
			// Control failure is data. Independent lane/run visibility remains usable.
			m.mu.Lock()
			s = m.laneControlSnapshotLocked(nil, false)
			m.mu.Unlock()
			fmt.Fprintln(errOut, laneCLIError(err))
			code = 1
		}
		if r.machine {
			fmt.Fprintln(out, toolText(s))
		} else {
			for _, v := range s.Lanes {
				fmt.Fprintf(out, "%s  %s; %s\n", cell(v.Name), v.Phase, cell(v.Explanation))
			}
		}
		return code
	}
	if r.operation == "pause" || r.operation == "resume" {
		if err := m.prepareControlStore(); err != nil {
			return fail(err)
		}
		result, err := m.setLaneControl(laneControlChange{Lane: r.lane, Paused: r.operation == "pause", AfterCurrent: r.after}, "cli")
		if err != nil {
			return fail(err)
		}
		word := "saved"
		if !result.Changed {
			word = "already saved"
		}
		fmt.Fprintf(out, "%s: lane hold %s (revision %d).\n", r.lane, word, result.Revision)
		if s, err := m.laneControls(); err == nil {
			for _, v := range s.Lanes {
				if v.Name == r.lane {
					fmt.Fprintln(out, v.Explanation)
				}
			}
		} else {
			fmt.Fprintln(out, "Current lane controls cannot be read; check lane list before starting new work.")
		}
		if r.operation == "resume" {
			fmt.Fprintln(out, "This resume started no work. Held messages still need explicit review.")
		}
		return 0
	}
	h, text, state, err := m.heldCLISelection(r.lane, r.run)
	if err != nil {
		return fail(err)
	}
	if r.operation == "held-review" {
		fmt.Fprintln(out, toolText(map[string]any{"lane": h.Lane, "run_id": h.RunID, "goal_id": h.GoalID, "revision": h.Revision, "content_sha256": h.ContentSHA256, "state": state, "text": bounded(Redact(text), 8192), "explanation": "This message stays held until you explicitly send or discard this content revision. Review text is redacted and limited to 8 KiB."}))
		return 0
	}
	if h.Revision != r.revision || h.ContentSHA256 != r.digest {
		return fail(fmt.Errorf("held_message_changed"))
	}
	if r.operation == "held-discard" {
		if err := m.discardHeld(h, nil); err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "Reviewed message discarded. This did not cancel any possible child run.")
		return 0
	}
	id, err := m.sendHeld(h, nil)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(out, "Reviewed message delivery confirmed for run %s. A receipt retry does not send it again.\n", shortRunID(id))
	return 0
}

func laneCLIError(err error) string {
	code := err.Error()
	if strings.HasPrefix(code, "frozen by owner since ") {
		code = "frozen"
	}
	var control *laneControlFault
	if errors.As(err, &control) {
		code = control.Code
	}
	var admission *laneAdmissionFault
	if errors.As(err, &admission) {
		if admission.Paused {
			message := "This lane is paused by you; new work was refused. Resume the lane before sending."
			for _, blocker := range admission.Blockers {
				if blocker == "frozen" {
					message += " All lanes are also frozen."
				}
				if blocker == "freeze_unavailable" {
					message += " The freeze state cannot be read safely."
				}
			}
			return message
		}
	}
	switch code {
	case "lane_not_found", "lane_not_configured", "unknown lane":
		return "This exact lane name is not configured. Only an existing retained hold can be resumed; configuration was not recreated."
	case "locked":
		return "Another operation holds the lane control lock. The change is not confirmed; read lane list before retrying."
	case "save_unconfirmed", "committed_audit_failed", "audit_unavailable":
		return "The durable change or its audit is uncertain. Read lane list before deciding what to do; no automatic retry was made."
	case "held_message_session_unavailable":
		return "The original message session is unavailable or its agent kind changed. Sending is refused; the message remains available for review or discard."
	case "held_message_launch_uncertain":
		return "Message delivery is uncertain. The reservation stays held; do not infer delivery from this response."
	case "held_message_changed":
		return "The reviewed message revision or digest changed. Review the original run again before sending or discarding."
	case "held_message_discarded":
		return "This reviewed message was already discarded; it cannot be sent."
	case "lane is busy":
		return "The lane still has a current run. Wait for that run to end before explicit message delivery."
	case "frozen":
		return "All lanes are frozen. Resume clears only the lane hold; sending still needs an explicit unfreeze."
	case "lane_control_capacity":
		return "The retained lane-control store is full. No saved hold was removed and the change was refused."
	default:
		return "Lane controls or the original message cannot be read safely. The operation is refused; saved intent and held content have not been reconciled by a read."
	}
}

func CLI(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	if len(args) == 2 && args[0] == "__nuntius_web_exec" {
		return webExec(args[1])
	}
	if code, handled := launchEntry(args); handled {
		return code
	}
	home, e := os.UserHomeDir()
	if e != nil {
		fmt.Fprintln(errOut, "cannot locate home directory")
		return 1
	}
	fs := flag.NewFlagSet("nuntius", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	config := fs.String("config", "", "owner configuration file")
	store := fs.String("store", "", "private data directory")
	version := fs.Bool("version", false, "show version")
	fs.BoolVar(version, "v", false, "show version")
	if e = fs.Parse(args); e != nil {
		fmt.Fprintln(errOut, "invalid command flags; use nuntius help")
		return 2
	}
	rest := fs.Args()
	if *version && len(rest) == 0 {
		fmt.Fprintln(out, "nuntius "+Version)
		return 0
	}
	if len(rest) == 0 {
		rest = []string{"help"}
	}
	if len(rest) == 1 && rest[0] == "version" {
		fmt.Fprintln(out, "nuntius "+Version)
		return 0
	}
	if len(rest) == 1 && rest[0] == "help" {
		fmt.Fprintln(out, "Idle monitoring: observe runs the opt-in alert_only daemon; monitoring [--since 24h] [--json] reads its five time partitions. Phone delivery is disabled. Observation is not execution compliance or complete project health.")
		fmt.Fprintln(out, goalCLIUsage)
		fmt.Fprintln(out, "Judge reads: queue [--state STATE] [--project ID] [--json] | report show ID [--section NAME] [--json] | tail LANE [--lines N] [--json]. Portable states: planned, blocked, ready, parked; repeat --state for OR. Portable project filters are unavailable.")
		fmt.Fprintln(out, "Lane holds: lane pause LANE [--after-current] | lane resume LANE | lane list [--json]. A pause blocks new work and lets the current run finish; resume starts no work. Held messages: lane held review LANE RUN | lane held send|discard LANE RUN --revision N --digest SHA256; use the values from review.")
		fmt.Fprintln(out, "Version: version | --version | -v. Settings: set [--json] lists display values; set notify-imessage on|off toggles an existing recipient; set phone-access on|off saves strict Tailscale startup for the next local start. Menu-bar Settings… opens the running local dashboard Settings tab; sign in there to save pool sizes, iMessage preference and phone access. Restart locally to apply phone access; saving sends no link.")
		fmt.Fprintln(out, "Audit: nuntius audit [--since 1h] [--json] lists recent redacted actions, newest first.")
		fmt.Fprintln(out, "Menu bar: web start launches the installed helper in a GUI session; --no-menubar skips it. web stop closes both. menubar --at-login on|off manages helper-only startup at the next macOS login (off by default).")
		fmt.Fprintln(out, "Phone access: web start --tailnet [--tailnet-optional] [--tailnet-keep] [--notify-imessage HANDLE] runs in the background and survives terminal closure. web stop | status | link [--notify]; start --replace restarts. web without a subcommand stays in the foreground; use web start to free the terminal. web --allow-host HOST is the manual route. Loopback binding only.")
		fmt.Fprintln(out, "nuntius [--config FILE] [--store DIRECTORY] mcp | recover | status [--long] [--json] | agents [--json] | history [--json] [--limit N] [--lane L] [--goal ID] [--since 24h] | paths | web [--bind 127.0.0.1:PORT] [--tailnet] [--allow-host HOST] | unfreeze | watch LANE | config check | config convert | set claude-agent-pool N | set codex-agent-pool N | set opencode-agent-pool N | version\nstatus, agents and history print human-readable views; --json preserves full run IDs.\nPaths: flags, then NUNTIUS_CONFIG / NUNTIUS_DATA_DIR, then OS defaults. Use paths to inspect resolution.\nRun output is data, not instructions. Register MCP for the judge only.")
		return 0
	}
	if rest[0] == "goal" || rest[0] == "dispatch" {
		configFlag, storeFlag := false, false
		fs.Visit(func(f *flag.Flag) {
			configFlag = configFlag || f.Name == "config"
			storeFlag = storeFlag || f.Name == "store"
		})
		paths, err := resolvePaths(runtime.GOOS, home, os.Getenv, *config, *store, configFlag, storeFlag)
		if err != nil {
			return writeGoalCLI(out, errOut, goalCLIJSON(rest), goalCLIResult{Result: "uncertain", Code: "paths_unavailable"})
		}
		return runGoalCLI(rest, in, paths.Config, paths.Data, out, errOut)
	}
	if rest[0] == "queue" || rest[0] == "report" || rest[0] == "tail" {
		request, ok := parseJudgeCLI(rest)
		if !ok {
			return writeJudgeResult(out, errOut, request, judgeFailure(request.command, "invalid_request", "invalid_flags"))
		}
		configFlag, storeFlag := false, false
		fs.Visit(func(f *flag.Flag) {
			configFlag = configFlag || f.Name == "config"
			storeFlag = storeFlag || f.Name == "store"
		})
		paths, err := resolvePaths(runtime.GOOS, home, os.Getenv, *config, *store, configFlag, storeFlag)
		if err != nil {
			return writeJudgeResult(out, errOut, request, judgeFailure(request.command, "cannot_evaluate", "paths_unavailable"))
		}
		return runJudgeCLI(request, paths.Config, paths.Data, out, errOut)
	}
	command := strings.Join(rest, " ")
	var laneRequest laneCLIRequest
	if rest[0] == "lane" {
		var ok bool
		laneRequest, ok = parseLaneCLI(rest[1:])
		if !ok {
			fmt.Fprintln(errOut, "Use lane pause LANE [--after-current], lane resume LANE, lane list [--json], or lane held review|send|discard LANE RUN. Send/discard require --revision N --digest SHA256 from review.")
			return 2
		}
	}
	if rest[0] != "lane" && command != "recover" && command != "config check" && command != "config convert" && command != "mcp" && rest[0] != "status" && command != "paths" && command != "unfreeze" && command != "agents" && command != "agents --json" && rest[0] != "history" && rest[0] != "audit" && rest[0] != "menubar" && rest[0] != "monitoring" && command != "observe" && !(len(rest) == 2 && rest[0] == "watch") && !(rest[0] == "web") && !(rest[0] == "set" && (len(rest) == 1 || (len(rest) <= 3 && (poolSetting(rest[1]) || rest[1] == "notify-imessage" || rest[1] == "phone-access")) || command == "set --json")) {
		fmt.Fprintln(errOut, "unknown command; use nuntius help")
		return 2
	}
	configFlag, storeFlag := false, false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			configFlag = true
		}
		if f.Name == "store" {
			storeFlag = true
		}
	})
	paths, e := resolvePaths(runtime.GOOS, home, os.Getenv, *config, *store, configFlag, storeFlag)
	if e != nil {
		fmt.Fprintln(errOut, e.Error())
		if command == "mcp" {
			return serveCLI(ctx, nil, in, out, errOut)
		}
		return 1
	}
	*config, *store = paths.Config, paths.Data
	if rest[0] == "menubar" {
		return menubarCommand(home, *store, *config, rest[1:], out, errOut)
	}
	if emptyStoreWarning(paths.Data, paths.DefaultData) {
		fmt.Fprintln(errOut, "warning: resolved data directory has no runs; the default directory holds runs; no data was moved")
	}
	if command == "paths" {
		fmt.Fprintf(out, "config: %s (%s)\ndata:   %s (%s)\n", cell(paths.Config), paths.ConfigSource, cell(paths.Data), paths.DataSource)
		return 0
	}
	c, e := LoadConfig(*config)
	if e != nil {
		fmt.Fprintln(errOut, Redact(e.Error()))
		if command == "mcp" {
			return serveCLI(ctx, nil, in, out, errOut)
		}
		return 1
	}
	if command == "config check" {
		fmt.Fprintln(out, "configuration valid")
		return 0
	}
	if command == "set --json" {
		fmt.Fprintln(out, toolText(settingsSnapshot(c)))
		return 0
	}
	if rest[0] == "set" && len(rest) < 3 {
		key := ""
		if len(rest) == 2 {
			key = rest[1]
		}
		writeSettings(out, c, key)
		return 0
	}
	var m *Manager
	if rest[0] == "lane" || rest[0] == "status" || rest[0] == "agents" || rest[0] == "monitoring" || command == "observe" || rest[0] == "audit" || rest[0] == "history" {
		m, e = newReadManager(c, *store)
	} else {
		m, e = NewManager(c, *store)
	}
	if e != nil {
		fmt.Fprintln(errOut, Redact(e.Error()))
		if command == "mcp" {
			return serveCLI(ctx, nil, in, out, errOut)
		}
		return 1
	}
	defer m.Close()
	switch rest[0] {
	case "observe":
		return m.observeCLI(ctx, idleGuardHost{}, out, errOut)
	case "monitoring":
		mf := flag.NewFlagSet("monitoring", flag.ContinueOnError)
		mf.SetOutput(io.Discard)
		machine := mf.Bool("json", false, "machine output")
		since := mf.Duration("since", 24*time.Hour, "report window up to 31 days")
		if mf.Parse(rest[1:]) != nil || len(mf.Args()) != 0 || *since <= 0 || *since > 31*24*time.Hour {
			fmt.Fprintln(errOut, "invalid monitoring flags")
			return 2
		}
		until := time.Now()
		lanes := make([]string, 0, len(c.Lanes))
		for _, lane := range c.Lanes {
			lanes = append(lanes, lane.Name)
		}
		r := readIdleGuard(m.store, lanes, until.Add(-*since), until)
		if *machine {
			fmt.Fprintln(out, toolText(r))
		} else {
			fmt.Fprintf(out, "alert_only observation; coverage=%s; phone delivery disabled; no execution compliance or complete project-health claim\n", r.Coverage)
			for _, lane := range r.Lanes {
				fmt.Fprintf(out, "%s running=%.0fs permitted-idle=%.0fs unexplained-idle=%.0fs blocked/no-demand=%.0fs unknown=%.0fs O=%.0fs U=%.0fs ratio=%s\n", cell(lane.Lane), lane.RunningSeconds, lane.PermittedIdleSeconds, lane.UnexplainedIdleSeconds, lane.BlockedNoDemandSeconds, lane.UnknownSeconds, lane.OpportunitySeconds, lane.UnexplainedSeconds, lane.RatioDisplay)
			}
		}
	case "lane":
		return m.laneCommand(laneRequest, out, errOut)
	case "audit":
		af := flag.NewFlagSet("audit", flag.ContinueOnError)
		af.SetOutput(io.Discard)
		machine := af.Bool("json", false, "machine output")
		since := af.String("since", "", "positive duration")
		if af.Parse(rest[1:]) != nil || len(af.Args()) != 0 {
			fmt.Fprintln(errOut, "invalid audit flags")
			return 2
		}
		rows, err := m.Audit(*since)
		if err != nil {
			fmt.Fprintln(errOut, Redact(err.Error()))
			return 2
		}
		if *machine {
			fmt.Fprintln(out, toolText(rows))
		} else if writeAudit(out, rows) != nil {
			fmt.Fprintln(errOut, "cannot write audit")
			return 1
		}
	case "recover":
		if e = m.Recover(); e != nil {
			fmt.Fprintln(errOut, Redact(e.Error()))
			return 1
		}
		fmt.Fprintln(out, "recovery checked; use status and history for interrupted and resumed goals")
	case "history":
		hf := flag.NewFlagSet("history", flag.ContinueOnError)
		hf.SetOutput(io.Discard)
		machine := hf.Bool("json", false, "machine output")
		filter := HistoryFilter{}
		hf.IntVar(&filter.Limit, "limit", 20, "last 1..200 finished runs")
		hf.StringVar(&filter.Lane, "lane", "", "exact lane")
		hf.StringVar(&filter.GoalID, "goal", "", "exact goal ID")
		hf.StringVar(&filter.Since, "since", "", "positive duration")
		if hf.Parse(rest[1:]) != nil || len(hf.Args()) != 0 {
			fmt.Fprintln(errOut, "invalid history flags")
			return 2
		}
		rows, err := m.History(filter)
		if err != nil {
			fmt.Fprintln(errOut, "history unavailable; check the run store and filters")
			return 2
		}
		if *machine {
			fmt.Fprintln(out, toolText(rows))
		} else if writeHistory(out, rows) != nil {
			fmt.Fprintln(errOut, "cannot write history")
			return 1
		}
	case "config", "set":
		if rest[0] == "set" && rest[1] == "phone-access" {
			if len(rest) != 3 || (rest[2] != "on" && rest[2] != "off") {
				fmt.Fprintln(errOut, "use set phone-access on|off")
				return 2
			}
			if e = m.SetPhoneAccess(*config, rest[2] == "on"); e != nil {
				fmt.Fprintln(errOut, "phone access setting unavailable; run nuntius config check")
				return 1
			}
			fmt.Fprintln(out, "phone access saved for the next local dashboard start; restart locally to apply")
			return 0
		}
		if rest[0] == "set" && rest[1] == "notify-imessage" {
			if len(rest) != 3 || (rest[2] != "on" && rest[2] != "off") {
				fmt.Fprintln(errOut, "use set notify-imessage on|off")
				return 2
			}
			if e = m.SetIMessage(*config, rest[2] == "on"); e != nil {
				fmt.Fprintln(errOut, Redact(e.Error()))
				return 1
			}
			fmt.Fprintln(out, "iMessage setting updated")
			return 0
		}
		kind := ""
		size := 0
		convert := rest[0] == "config"
		if !convert {
			kind = settingKind(rest[1])
			size, e = strconv.Atoi(rest[2])
			if e != nil {
				fmt.Fprintln(errOut, "pool size must be 0..8")
				return 2
			}
		}
		if e = m.SetPool(*config, kind, size, convert); e != nil {
			fmt.Fprintln(errOut, Redact(e.Error()))
			return 1
		}
		fmt.Fprintln(out, "pool configuration updated; agents reload it on their next check")
	case "unfreeze":
		if e = m.Unfreeze(); e != nil {
			fmt.Fprintln(errOut, Redact(e.Error()))
			return 1
		}
		fmt.Fprintln(out, "unfrozen")
	case "agents":
		agents, e := m.Agents()
		if e != nil {
			fmt.Fprintln(errOut, Redact(e.Error()))
			return 1
		}
		if len(rest) == 2 {
			fmt.Fprintln(out, toolText(agents))
		} else {
			for _, a := range agents {
				state := a.State
				if a.DisplayState != "" {
					state = a.DisplayState
				}
				fmt.Fprintf(out, "%s  %s  %s  flags=%s  %s  goal=%s model=%s effort=%s; %s\n", cell(a.Name), cell(a.Kind), cell(a.StartMode), toolText(a.Flags), cell(state), cell(a.Goal), identityValue(a.Model), identityValue(a.Effort), cell(a.LaneControl.Explanation))
			}
		}
	case "web":
		return m.webCommand(ctx, rest[1:], out, errOut)
	case "mcp":
		return serveCLI(ctx, m, in, out, errOut)
	case "status":
		sf := flag.NewFlagSet("status", flag.ContinueOnError)
		sf.SetOutput(io.Discard)
		long := sf.Bool("long", false, "show current action")
		machine := sf.Bool("json", false, "machine output with full run IDs")
		if sf.Parse(rest[1:]) != nil || len(sf.Args()) != 0 {
			fmt.Fprintln(errOut, "invalid status flags")
			return 2
		}
		s, e := m.StatusView()
		if e != nil {
			fmt.Fprintln(errOut, Redact(e.Error()))
			return 1
		}
		if *machine {
			fmt.Fprintln(out, toolText(s))
		} else if e := m.writeStatus(out, s, *long); e != nil {
			fmt.Fprintln(errOut, "cannot write status")
			return 1
		}
	case "watch":
		previous := ""
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			rows, e := m.Tail(rest[1], 200)
			if e != nil {
				fmt.Fprintln(errOut, Redact(e.Error()))
				return 1
			}
			text := strings.Join(rows, "\n")
			if text != previous {
				fmt.Fprintln(out, text)
				previous = text
			}
			s, e := m.Status()
			if e != nil {
				fmt.Fprintln(errOut, Redact(e.Error()))
				return 1
			}
			for _, v := range s {
				if v.Lane == rest[1] && v.State != "running" {
					return 0
				}
			}
			select {
			case <-ctx.Done():
				return 0
			case <-tick.C:
			}
		}
	}
	return 0
}

func (m *Manager) observeCLI(ctx context.Context, host idleGuardHost, out, errOut io.Writer) int {
	fault := make(chan string, 1)
	contention := make(chan struct{}, 1)
	host.onFault = func() {
		m.mu.Lock()
		cause := m.idleGuardFault
		m.mu.Unlock()
		if cause == "reader_contention" {
			select {
			case contention <- struct{}{}:
			default:
			}
			return
		}
		// A pending transient notice must never hide a terminal fault.
		select {
		case fault <- cause:
		default:
		}
	}
	if m.startIdleGuard(ctx, host) != nil {
		fmt.Fprintln(errOut, "idle observer unavailable; select alert_only locally and check the private store and controller owner")
		return 1
	}
	fmt.Fprintln(out, "alert_only observer running; phone delivery disabled; no execution or project-health claim")
	for {
		select {
		case <-contention:
			fmt.Fprintln(errOut, "idle monitoring reader contention; missed coverage is unknown; observation will retry")
		case cause := <-fault:
			if cause == "storage_limit" {
				fmt.Fprintln(errOut, "idle monitoring storage limit reached; coverage is unknown; preserve records for owner review")
			} else {
				fmt.Fprintln(errOut, "idle monitoring persistence unavailable; coverage is unknown; preserve records for owner review")
			}
			return 1
		case <-ctx.Done():
			return 0
		}
	}
}

func serveCLI(ctx context.Context, m *Manager, in io.Reader, out, errOut io.Writer) int {
	done := make(chan struct{})
	defer close(done)
	if closer, ok := in.(io.ReadCloser); ok {
		go func() {
			select {
			case <-ctx.Done():
				_ = closer.Close()
			case <-done:
			}
		}()
	}
	if e := Serve(ctx, m, in, out); e != nil && ctx.Err() == nil {
		fmt.Fprintln(errOut, Redact(e.Error()))
		return 1
	}
	return 0
}
