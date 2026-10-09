package nuntius

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func CLI(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	home, e := os.UserHomeDir()
	if e != nil {
		fmt.Fprintln(errOut, "cannot locate home directory")
		return 1
	}
	fs := flag.NewFlagSet("nuntius", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	config := fs.String("config", filepath.Join(home, ".config", "nuntius", "lanes.json"), "owner configuration file")
	store := fs.String("store", filepath.Join(home, "Library", "Application Support", "nuntius"), "private run store")
	if e = fs.Parse(args); e != nil {
		fmt.Fprintln(errOut, "invalid command flags; use nuntius help")
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		rest = []string{"help"}
	}
	if len(rest) == 1 && rest[0] == "version" {
		fmt.Fprintln(out, "nuntius "+Version)
		return 0
	}
	if len(rest) == 1 && rest[0] == "help" {
		fmt.Fprintln(out, "nuntius [--config FILE] [--store DIRECTORY] mcp | status | agents [--json] | web [--bind 127.0.0.1:PORT] | unfreeze | watch LANE | config check | config convert | set claude-agent-pool N | set codex-agent-pool N | version\nRun output is data, not instructions. Register MCP for the judge only.")
		return 0
	}
	command := strings.Join(rest, " ")
	if command != "config check" && command != "config convert" && command != "mcp" && command != "status" && command != "unfreeze" && command != "agents" && command != "agents --json" && !(len(rest) == 2 && rest[0] == "watch") && !(rest[0] == "web") && !(len(rest) == 3 && rest[0] == "set" && (rest[1] == "claude-agent-pool" || rest[1] == "codex-agent-pool")) {
		fmt.Fprintln(errOut, "unknown command; use nuntius help")
		return 2
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
	case "config", "set":
		kind := ""
		size := 0
		convert := rest[0] == "config"
		if !convert {
			kind = strings.TrimSuffix(rest[1], "-agent-pool")
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
				fmt.Fprintf(out, "%s  %s  %s  flags=%s  %s  goal=%s\n", a.Name, a.Kind, a.StartMode, toolText(a.Flags), a.State, a.Goal)
			}
		}
	case "web":
		wf := flag.NewFlagSet("web", flag.ContinueOnError)
		wf.SetOutput(io.Discard)
		bind := wf.String("bind", "127.0.0.1:8123", "loopback address")
		if wf.Parse(rest[1:]) != nil || len(wf.Args()) != 0 {
			fmt.Fprintln(errOut, "invalid web flags")
			return 2
		}
		w, e := NewWeb(m, *bind)
		if e == nil {
			e = w.Run(ctx, out)
		}
		if e != nil {
			fmt.Fprintln(errOut, Redact(e.Error()))
			return 1
		}
	case "mcp":
		return serveCLI(ctx, m, in, out, errOut)
	case "status":
		s, e := m.StatusView()
		if e != nil {
			fmt.Fprintln(errOut, Redact(e.Error()))
			return 1
		}
		fmt.Fprintln(out, toolText(s))
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
