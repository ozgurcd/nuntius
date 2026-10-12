// testagent is an offline fixture. It never invokes an agent or the network.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// fakeTailscale records every invocation and answers from files in the
// directory named by NUNTIUS_FAKE_TAILSCALE: status.json, serve.json (absent
// means no serve config) and serve-fail (refuse to create a mapping).
func fakeTailscale() {
	dir := os.Getenv("NUNTIUS_FAKE_TAILSCALE")
	if dir == "" {
		os.Exit(3)
	}
	args := strings.Join(os.Args[1:], " ")
	log, err := os.OpenFile(filepath.Join(dir, "calls.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(3)
	}
	fmt.Fprintln(log, args)
	log.Close()
	switch {
	case args == "status --json":
		b, err := os.ReadFile(filepath.Join(dir, "status.json"))
		if err != nil {
			os.Exit(1)
		}
		os.Stdout.Write(b)
	case args == "serve status --json":
		if _, err := os.Stat(filepath.Join(dir, "serve-status-fail")); err == nil {
			os.Exit(1)
		}
		b, err := os.ReadFile(filepath.Join(dir, "serve.json"))
		if err != nil {
			b = []byte("null\n")
		}
		os.Stdout.Write(b)
	case strings.HasPrefix(args, "serve --bg --https=443 localhost:"):
		if _, err := os.Stat(filepath.Join(dir, "serve-fail")); err == nil {
			fmt.Fprintln(os.Stderr, "fixture serve failure")
			os.Exit(1)
		}
		var status struct {
			Self struct{ DNSName string }
		}
		b, _ := os.ReadFile(filepath.Join(dir, "status.json"))
		_ = json.Unmarshal(b, &status)
		host := strings.TrimSuffix(status.Self.DNSName, ".")
		port := strings.TrimPrefix(args, "serve --bg --https=443 localhost:")
		config := map[string]any{"TCP": map[string]any{"443": map[string]any{"HTTPS": true}}, "Web": map[string]any{host + ":443": map[string]any{"Handlers": map[string]any{"/": map[string]any{"Proxy": "http://127.0.0.1:" + port}}}}}
		b, _ = json.Marshal(config)
		if err := os.WriteFile(filepath.Join(dir, "serve.json"), b, 0600); err != nil {
			os.Exit(1)
		}
		if partial, err := os.ReadFile(filepath.Join(dir, "serve-partial-fail")); err == nil {
			if len(partial) > 0 {
				_ = os.WriteFile(filepath.Join(dir, "serve.json"), partial, 0600)
			}
			if _, err := os.Stat(filepath.Join(dir, "serve-status-fail-after-create")); err == nil {
				_ = os.WriteFile(filepath.Join(dir, "serve-status-fail"), nil, 0600)
			}
			os.Exit(1)
		}
		fmt.Println("Available within your tailnet:")
	case args == "serve --https=443 off":
		if _, err := os.Stat(filepath.Join(dir, "serve-off-fail")); err == nil {
			os.Exit(1)
		}
		if err := os.WriteFile(filepath.Join(dir, "serve.json"), []byte("null\n"), 0600); err != nil {
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "fixture tailscale: unexpected arguments")
		os.Exit(2)
	}
}

// fakeOsascript records its argv as one JSON line and never opens Messages.
func fakeOsascript() {
	dir := os.Getenv("NUNTIUS_FAKE_TAILSCALE")
	if dir == "" {
		os.Exit(3)
	}
	log, err := os.OpenFile(filepath.Join(dir, "osascript.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(3)
	}
	args := append([]string(nil), os.Args[1:]...)
	if len(args) == 1 && args[0] == "-" {
		input, e := io.ReadAll(io.LimitReader(os.Stdin, 8192))
		if e != nil || !strings.Contains(string(input), "set recipientHandle to") || !strings.Contains(string(input), "send messageText") {
			os.Exit(3)
		}
		if expected := os.Getenv("NUNTIUS_FAKE_IMESSAGE_HANDLE"); expected != "" && !strings.Contains(string(input), "set recipientHandle to \""+expected+"\"") {
			os.Exit(3)
		}
	}
	for i, arg := range args {
		if at := strings.Index(arg, "https://"); at >= 0 && strings.Contains(arg, "?token=") {
			args[i] = arg[:at] + "[private sign-in link]"
		}
	}
	b, _ := json.Marshal(args)
	fmt.Fprintln(log, string(b))
	log.Close()
	if _, err := os.Stat(filepath.Join(dir, "osascript-fail")); err == nil {
		fmt.Fprintln(os.Stderr, "fixture osascript failure")
		os.Exit(1)
	}
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "brief-pointer" {
		prompt, err := io.ReadAll(io.LimitReader(os.Stdin, 12001))
		if err != nil || len(prompt) > 12000 {
			os.Exit(31)
		}
		match := regexp.MustCompile(`Read the full sealed brief at ("(?:[^"\\]|\\.)*")\. Follow`).FindSubmatch(prompt)
		if len(match) != 2 {
			os.Exit(32)
		}
		path, err := strconv.Unquote(string(match[1]))
		if err != nil {
			os.Exit(33)
		}
		f, err := os.Open(path)
		if err != nil {
			os.Exit(34)
		}
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() > 128*1024 {
			f.Close()
			os.Exit(35)
		}
		full, err := io.ReadAll(io.LimitReader(f, 128*1024+1))
		closeErr := f.Close()
		if err != nil || closeErr != nil || int64(len(full)) != st.Size() {
			os.Exit(36)
		}
		fmt.Printf("%x %d\n", sha256.Sum256(full), len(full))
		return
	}
	if os.Getenv("NUNTIUS_FIXTURE_WEB_BINARY") != "" && len(os.Args) > 1 &&
		(os.Args[1] == "__fixture_web" || os.Args[1] == "__nuntius_web_exec" || os.Args[1] == "--config") {
		args := os.Args[1:]
		if args[0] == "__fixture_web" {
			args = args[1:]
		}
		fixtureWeb(args)
		return
	}
	// Invoked through a symlink named like the real program, the fixture stands
	// in for the Tailscale CLI or osascript. It never contacts either.
	switch filepath.Base(os.Args[0]) {
	case "tailscale":
		fakeTailscale()
		return
	case "osascript":
		fakeOsascript()
		return
	case "launchctl":
		fakeLaunchctl()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "run" {
		opencode()
		return
	}
	// Like Codex, exec-only sandbox options are not resume options.
	if len(os.Args) > 1 && os.Args[1] == "exec" {
		resumed := false
		for _, arg := range os.Args[2:] {
			if arg == "resume" {
				resumed = true
			}
			if resumed && (arg == "--sandbox" || strings.HasPrefix(arg, "--sandbox=")) {
				os.Exit(2)
			}
		}
	}
	b, _ := io.ReadAll(os.Stdin)
	mode := string(b)
	if strings.Contains(mode, "GOAL: DISPATCH (size S)") {
		cwd, _ := os.Getwd()
		full := b
		match := regexp.MustCompile(`Read the full sealed brief at ("(?:[^"\\]|\\.)*")\. Follow`).FindSubmatch(b)
		if len(match) == 2 {
			path, err := strconv.Unquote(string(match[1]))
			if err != nil {
				os.Exit(41)
			}
			full, err = os.ReadFile(path)
			if err != nil || len(full) > 128*1024 {
				os.Exit(42)
			}
		}
		evidence := fmt.Sprintf("%x %x %d", sha256.Sum256(b), sha256.Sum256(full), len(full))
		if os.WriteFile(filepath.Join(cwd, fmt.Sprintf("native-start-%d", os.Getpid())), []byte(evidence), 0600) != nil {
			os.Exit(43)
		}
		mode = string(full)
	}
	if strings.Contains(mode, "ack-goal") {
		fmt.Println(`{"type":"system","session_id":"fixture-session"}`)
		if strings.Contains(mode, "ack-barrier") {
			fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"ready"}]}}`)
			cwd, _ := os.Getwd()
			for {
				if _, err := os.Stat(filepath.Join(cwd, "admission-release")); err == nil {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"Goal accepted"}]}}`)
		fmt.Println(`{"type":"result","is_error":false}`)
		return
	}
	args, _ := json.Marshal(os.Args[1:])
	cwd, _ := os.Getwd()
	if !strings.Contains(mode, "no-session") {
		if len(os.Args) > 1 && os.Args[1] == "exec" {
			fmt.Println(`{"type":"thread.started","thread_id":"fixture-thread"}`)
		} else {
			fmt.Println(`{"type":"system","session_id":"fixture-session"}`)
		}
	}
	emit := func(text string) {
		v, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]string{"type": "text", "text": text}, map[string]string{"type": "tool_use", "name": "Read"}}}})
		fmt.Println(string(v))
	}
	emit("args=" + string(args))
	emit("cwd=" + cwd)
	if marker := os.Getenv("NUNTIUS_FIXTURE_ENV"); marker == "inherited-fixture-marker" {
		emit("env=" + marker)
	}
	emit(fmt.Sprintf("ppid=%d", os.Getppid()))
	fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"codex answer"}}`)
	fmt.Println(`{"type":"item.completed","item":{"type":"command_execution","command":"fixture command","aggregated_output":"must-not-return"}}`)
	emit("done")
	if strings.Contains(mode, "admission-barrier") {
		emit("ready")
		for {
			if _, err := os.Stat(filepath.Join(cwd, "admission-release")); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err := os.WriteFile(filepath.Join(cwd, "ADMISSION-RUN.txt"), []byte("Normal fixture report\n"), 0600); err != nil {
			os.Exit(24)
		}
		return
	}
	if strings.Contains(mode, "orphan-detect") {
		parent := os.Getppid()
		emit("ready")
		for os.Getppid() == parent {
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(23)
	}
	if strings.Contains(mode, "slow-finish") {
		emit("ready")
		time.Sleep(750 * time.Millisecond)
		emit("finished after client exit")
	}
	if strings.HasPrefix(mode, "Previous run ended before report delivery; exit cause unknown at ") {
		emit("recovery-message=" + mode)
	}
	if strings.Contains(mode, "ignore-int") {
		signal.Ignore(os.Interrupt)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"ready"}]}}`)
		for {
			time.Sleep(time.Hour)
		}
	}
	if strings.Contains(mode, "wait") {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"ready"}]}}`)
		<-ch
		return
	}
	if strings.Contains(mode, "exit-seven") {
		os.Exit(7)
	}
}

// This fixture, unlike launchd, must explicitly reap the child it starts.
// A stop file works even before the dashboard has saved its process identity.
// The watchdog also bounds orphan lifetime if the test runner crashes.
func fixtureWeb(args []string) {
	dir := os.Getenv("NUNTIUS_FIXTURE_WEB_DIR")
	name := filepath.Join(dir, strconv.Itoa(os.Getpid()))
	if os.WriteFile(name+".started", nil, 0600) != nil {
		return
	}
	defer func() { _ = os.WriteFile(name+".reaped", nil, 0600) }()
	cmd := exec.Command(os.Getenv("NUNTIUS_FIXTURE_WEB_BINARY"), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() != nil {
		return
	}
	_ = os.WriteFile(name+".pid", []byte(strconv.Itoa(cmd.Process.Pid)), 0600)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		select {
		case <-done:
			return
		case <-deadline.C:
			_ = cmd.Process.Kill()
			<-done
			return
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
				_ = cmd.Process.Kill()
				<-done
				return
			}
		}
	}
}

// Only private fixture directories are used; this never invokes launchd.
func fakeLaunchctl() {
	dir := os.Getenv("NUNTIUS_FAKE_LAUNCHCTL")
	if dir == "" {
		os.Exit(3)
	}
	args := os.Args[1:]
	log, err := os.OpenFile(filepath.Join(dir, "launchctl.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(3)
	}
	fmt.Fprintln(log, strings.Join(args, " "))
	log.Close()
	path := filepath.Join(dir, "job.path")
	pidPath := filepath.Join(dir, "job.pid")
	switch args[0] {
	case "bootstrap":
		if len(args) != 3 {
			os.Exit(2)
		}
		if _, err := os.Stat(path); err == nil {
			os.Exit(1)
		}
		if os.WriteFile(path, []byte(args[2]), 0600) != nil {
			os.Exit(1)
		}
	case "kickstart":
		if _, err := os.Stat(filepath.Join(dir, "kickstart-fail")); err == nil {
			os.Exit(1)
		}
		p, err := os.ReadFile(path)
		if err != nil {
			os.Exit(1)
		}
		b, err := os.ReadFile(string(p))
		if err != nil {
			os.Exit(1)
		}
		var plist struct {
			Dict struct {
				Inner string `xml:",innerxml"`
			} `xml:"dict"`
		}
		if xml.Unmarshal(b, &plist) != nil {
			os.Exit(1)
		}
		dec := xml.NewDecoder(strings.NewReader(plist.Dict.Inner))
		var argv []string
		output := ""
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			el, ok := tok.(xml.StartElement)
			if !ok {
				continue
			}
			if el.Name.Local == "key" {
				var key string
				_ = dec.DecodeElement(&key, &el)
				if key == "ProgramArguments" {
					for {
						tok, _ = dec.Token()
						if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "string" {
							var s string
							_ = dec.DecodeElement(&s, &start)
							argv = append(argv, s)
						}
						if end, ok := tok.(xml.EndElement); ok && end.Name.Local == "array" {
							break
						}
					}
				}
				if key == "StandardOutPath" {
					for {
						tok, _ = dec.Token()
						if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "string" {
							_ = dec.DecodeElement(&output, &start)
							break
						}
					}
				}
			}
		}
		if len(argv) == 0 {
			os.Exit(1)
		}
		f, err := os.OpenFile(output, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(1)
		}
		defer f.Close()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = f, f
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if cmd.Start() != nil {
			os.Exit(1)
		}
		pid := strconv.Itoa(cmd.Process.Pid)
		if dir := os.Getenv("NUNTIUS_FIXTURE_WEB_DIR"); dir != "" {
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
				if b, err := os.ReadFile(filepath.Join(dir, pid+".pid")); err == nil && len(b) > 0 {
					pid = string(b)
					break
				}
				time.Sleep(time.Millisecond)
			}
		}
		_ = os.WriteFile(pidPath, []byte(pid), 0600)
		fmt.Println(pid)
		_ = cmd.Process.Release()
		if _, err := os.Stat(filepath.Join(dir, "kickstart-after-start-fail")); err == nil {
			os.Exit(1)
		}
	case "print":
		if _, err := os.Stat(path); err != nil {
			os.Exit(113)
		}
		b, _ := os.ReadFile(pidPath)
		if len(b) > 0 {
			fmt.Println("pid = " + string(b))
		} else {
			fmt.Println("state = not running")
		}
	case "bootout":
		_ = os.Remove(path)
		_ = os.Remove(pidPath)
	default:
		os.Exit(2)
	}
}

func opencode() {
	args := os.Args[1:]
	if len(args) < 5 || args[0] != "run" || args[1] != "--format" || args[2] != "json" || args[3] != "--model" || args[4] != "opencode/fixture-free" {
		os.Exit(2)
	}
	for i := 5; i < len(args); i++ {
		key, _, eq := strings.Cut(args[i], "=")
		if key != "--session" && key != "--agent" && key != "--variant" {
			os.Exit(2)
		}
		if !eq {
			i++
			if i >= len(args) {
				os.Exit(2)
			}
		}
	}
	b, _ := io.ReadAll(os.Stdin)
	mode := string(b)
	emit := func(kind string, part any) {
		v := map[string]any{"type": kind, "timestamp": time.Now().UnixMilli(), "part": part}
		if !strings.Contains(mode, "no-session") {
			v["sessionID"] = "ses_fixture"
		}
		data, _ := json.Marshal(v)
		fmt.Println(string(data))
	}
	text := func(s string) { emit("text", map[string]any{"type": "text", "text": s}) }
	data, _ := json.Marshal(args)
	text("args=" + string(data))
	cwd, _ := os.Getwd()
	text("cwd=" + cwd)
	emit("step_start", map[string]any{"type": "step-start"})
	emit("tool_use", map[string]any{"type": "tool", "tool": "bash", "callID": "fixture-call", "state": map[string]any{"status": "running", "input": map[string]any{"command": "fixture command"}}})
	if strings.Contains(mode, "ignore-int") && !strings.Contains(mode, "terminal-hold") {
		signal.Ignore(os.Interrupt)
		text("ready")
		for {
			time.Sleep(time.Hour)
		}
	}
	if strings.Contains(mode, "wait") {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		text("ready")
		<-ch
	}
	if strings.Contains(mode, "slow-finish") {
		text("ready")
		time.Sleep(750 * time.Millisecond)
	}
	if strings.HasPrefix(mode, "Previous run ended before report delivery; exit cause unknown at ") {
		text("recovery-message=" + mode)
	}
	emit("tool_use", map[string]any{"type": "tool", "tool": "bash", "callID": "fixture-call", "state": map[string]any{"status": "completed", "input": map[string]any{"command": "fixture command"}, "output": "must-not-return"}})
	text("done")
	if strings.Contains(mode, "terminal-hold") {
		if strings.Contains(mode, "ignore-int") {
			signal.Ignore(os.Interrupt)
		}
		text("ready")
	}
	emit("step_finish", map[string]any{"type": "step-finish", "reason": "stop"})
	if strings.Contains(mode, "terminal-hold") {
		for {
			if _, err := os.Stat("terminal-release"); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if strings.Contains(mode, "exit-seven") {
		os.Exit(7)
	}
}
