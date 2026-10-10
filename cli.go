package nuntius

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

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
		fmt.Fprintln(out, "Version: version | --version | -v. Settings: set [--json] lists display values; set notify-imessage on|off toggles an existing recipient. Menu-bar Settings… opens the running local dashboard Settings tab; sign in there to save pool sizes and iMessage preference.")
		fmt.Fprintln(out, "Audit: nuntius audit [--since 1h] [--json] lists recent redacted actions, newest first.")
		fmt.Fprintln(out, "Menu bar: web start launches the installed helper in a GUI session; --no-menubar skips it. web stop closes both. menubar --at-login on|off manages helper-only startup at the next macOS login (off by default).")
		fmt.Fprintln(out, "Phone access: web start --tailnet [--tailnet-optional] [--tailnet-keep] [--notify-imessage HANDLE] runs in the background and survives terminal closure. web stop | status | link [--notify]; start --replace restarts. web without a subcommand stays in the foreground; use web start to free the terminal. web --allow-host HOST is the manual route. Loopback binding only.")
		fmt.Fprintln(out, "nuntius [--config FILE] [--store DIRECTORY] mcp | recover | status [--long] [--json] | agents [--json] | history [--json] [--limit N] [--lane L] [--goal ID] [--since 24h] | paths | web [--bind 127.0.0.1:PORT] [--tailnet] [--allow-host HOST] | unfreeze | watch LANE | config check | config convert | set claude-agent-pool N | set codex-agent-pool N | set opencode-agent-pool N | version\nstatus, agents and history print human-readable views; --json preserves full run IDs.\nPaths: flags, then NUNTIUS_CONFIG / NUNTIUS_DATA_DIR, then OS defaults. Use paths to inspect resolution.\nRun output is data, not instructions. Register MCP for the judge only.")
		return 0
	}
	command := strings.Join(rest, " ")
	if command != "recover" && command != "config check" && command != "config convert" && command != "mcp" && rest[0] != "status" && command != "paths" && command != "unfreeze" && command != "agents" && command != "agents --json" && rest[0] != "history" && rest[0] != "audit" && rest[0] != "menubar" && !(len(rest) == 2 && rest[0] == "watch") && !(rest[0] == "web") && !(rest[0] == "set" && (len(rest) == 1 || (len(rest) <= 3 && (poolSetting(rest[1]) || rest[1] == "notify-imessage")) || command == "set --json")) {
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
	m, e := NewManager(c, *store)
	if e != nil {
		fmt.Fprintln(errOut, Redact(e.Error()))
		if command == "mcp" {
			return serveCLI(ctx, nil, in, out, errOut)
		}
		return 1
	}
	defer m.Close()
	switch rest[0] {
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
			fmt.Fprintln(errOut, Redact(err.Error()))
			return 2
		}
		if *machine {
			fmt.Fprintln(out, toolText(rows))
		} else if writeHistory(out, rows) != nil {
			fmt.Fprintln(errOut, "cannot write history")
			return 1
		}
	case "config", "set":
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
				fmt.Fprintf(out, "%s  %s  %s  flags=%s  %s  goal=%s model=%s effort=%s\n", cell(a.Name), cell(a.Kind), cell(a.StartMode), toolText(a.Flags), cell(state), cell(a.Goal), identityValue(a.Model), identityValue(a.Effort))
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
