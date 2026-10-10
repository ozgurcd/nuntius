// testagent is an offline fixture. It never invokes an agent or the network.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
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
	if strings.Contains(mode, "ack-goal") {
		fmt.Println(`{"type":"system","session_id":"fixture-session"}`)
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
	fmt.Println(`{"type":"item.completed","item":{"type":"command_execution","command":"must-not-return","aggregated_output":"must-not-return"}}`)
	emit("done")
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
	if strings.HasPrefix(mode, "You were interrupted by a restart or crash at ") {
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
