package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Exercise the actual newline-delimited Serve transport, not callContext.
func laneMCPExchange(t *testing.T, m *Manager, requests ...string) []map[string]any {
	t.Helper()
	input := []string{`{"jsonrpc":"2.0","id":1,"method":"initialize"}`, `{"jsonrpc":"2.0","method":"notifications/initialized"}`}
	input = append(input, requests...)
	var out bytes.Buffer
	if err := Serve(context.Background(), m, strings.NewReader(strings.Join(input, "\n")+"\n"), &out); err != nil {
		t.Fatal("in-memory stdio exchange failed")
	}
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte{'\n'})
	if len(lines) != len(requests)+1 {
		t.Fatal("stdio response cardinality")
	}
	responses := make([]map[string]any, 0, len(requests))
	for _, line := range lines[1:] {
		var response map[string]any
		if json.Unmarshal(line, &response) != nil || response["jsonrpc"] != "2.0" {
			t.Fatal("invalid stdio response")
		}
		responses = append(responses, response)
	}
	return responses
}

func laneMCPRequest(name, arguments string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, arguments)
}

func laneMCPResult(t *testing.T, response map[string]any, failed bool) map[string]any {
	t.Helper()
	if response["error"] != nil || response["id"] != float64(2) {
		t.Fatal("application failure became a JSON-RPC envelope error")
	}
	result, ok := response["result"].(map[string]any)
	if !ok || result["isError"] != failed {
		t.Fatal("application success/error flag")
	}
	return result
}

func laneMCPStructured(t *testing.T, response map[string]any, failed bool) map[string]any {
	t.Helper()
	result := laneMCPResult(t, response, failed)
	structured, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatal("typed structured result absent")
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatal("text fallback cardinality")
	}
	entry, ok := content[0].(map[string]any)
	if !ok || entry["type"] != "text" {
		t.Fatal("text fallback absent")
	}
	text, _ := entry["text"].(string)
	b, _ := json.Marshal(structured)
	if string(b) != text {
		t.Fatal("structured content differs from text JSON")
	}
	if explanation, _ := structured["explanation"].(string); len(explanation) < 20 {
		t.Fatal("result has no plain reason")
	}
	return structured
}

func TestLaneControlMCPInventory(t *testing.T) {
	m, _ := laneControlCommitFixture(t)
	responses := laneMCPExchange(t, m, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list := responses[0]["result"].(map[string]any)["tools"].([]any)
	wantNames := []string{"nuntius_web_ensure", "nuntius_status", "nuntius_history", "nuntius_signals", "nuntius_signals_ack", "nuntius_dispatch", "nuntius_reply", "nuntius_tail", "nuntius_report", "nuntius_cancel", "nuntius_freeze", "nuntius_unfreeze", "nuntius_owner_notes", "nuntius_owner_notes_ack", "nuntius_lane_pause", "nuntius_lane_resume"}
	got := map[string]map[string]any{}
	for _, row := range list {
		v := row.(map[string]any)
		name := v["name"].(string)
		if got[name] != nil {
			t.Fatal("duplicate tool")
		}
		got[name] = v
	}
	if len(got) != len(wantNames) {
		t.Fatal("complete tool inventory differs")
	}
	for _, name := range wantNames {
		if got[name] == nil {
			t.Fatal("required tool missing")
		}
	}
	for _, name := range []string{"nuntius_lane_pause", "nuntius_lane_resume"} {
		v := got[name]
		annotations := map[string]any{"readOnlyHint": false, "idempotentHint": true, "openWorldHint": false, "destructiveHint": true}
		if !reflect.DeepEqual(v["annotations"], annotations) || !strings.Contains(strings.ToLower(v["description"].(string)), "ask") {
			t.Fatal("conservative annotations or Ask guidance changed")
		}
		props := map[string]any{
			"lane":              map[string]any{"type": "string", "pattern": namePattern.String()},
			"expected_revision": map[string]any{"type": "integer", "minimum": float64(0), "maximum": float64(laneControlMaxRevision)},
		}
		if name == "nuntius_lane_pause" {
			props["after_current"] = map[string]any{"type": "boolean", "default": false}
		}
		want := map[string]any{"type": "object", "properties": props, "required": []any{"lane", "expected_revision"}, "additionalProperties": false}
		if !reflect.DeepEqual(v["inputSchema"], want) {
			t.Fatal("approved closed schema differs")
		}
	}
}

func TestLaneControlMCPStrictArguments(t *testing.T) {
	bad := []string{
		`{}`, `null`, `[]`, `true`, `{"lane":"codex"}`, `{"expected_revision":0}`,
		`{"lane":null,"expected_revision":0}`, `{"lane":1,"expected_revision":0}`, `{"lane":"../codex","expected_revision":0}`,
		`{"lane":"codex","expected_revision":null}`, `{"lane":"codex","expected_revision":"0"}`, `{"lane":"codex","expected_revision":false}`,
		`{"lane":"codex","expected_revision":0.5}`, `{"lane":"codex","expected_revision":0.0}`, `{"lane":"codex","expected_revision":1e0}`,
		`{"lane":"codex","expected_revision":-1}`, `{"lane":"codex","expected_revision":9007199254740992}`,
		`{"lane":"codex","lane":"claude","expected_revision":0}`, `{"lane":"codex","expected_revision":0,"expected_revision":0}`,
		`{"lane":"codex","expected_revision":0,"unknown":"do not echo this marker"}`,
		`{"lane":"codex","expected_revision":0,"source":"cli"}`, `{"lane":"codex","expected_revision":0,"caller_role":"owner"}`,
		`{"lane":"` + strings.Repeat("x", 65) + `","expected_revision":0}`,
		`{"lane":"codex","expected_revision":0` + strings.Repeat(" ", 4096) + `}`,
	}
	for _, name := range []string{"nuntius_lane_pause", "nuntius_lane_resume"} {
		for i, args := range bad {
			t.Run(fmt.Sprintf("%s/%d", name, i), func(t *testing.T) {
				m, store := laneControlCommitFixture(t)
				before := admissionArtifacts(t, store)
				v := laneMCPStructured(t, laneMCPExchange(t, m, laneMCPRequest(name, args))[0], true)
				if v["code"] != "invalid_request" || strings.Contains(string(safeJSON(v)), "do not echo") {
					t.Fatal("invalid request did not produce safe fixed refusal")
				}
				if !reflect.DeepEqual(before, admissionArtifacts(t, store)) {
					t.Fatal("invalid input changed artifacts")
				}
				if state := laneControlRaw(t, store); state.Revision != 0 || len(state.Lanes) != 0 || len(laneControlAuditRows(t, store)) != 0 {
					t.Fatal("invalid input changed controls or audit")
				}
			})
		}
	}
	for _, flag := range []string{`null`, `"true"`, `1`, `0`, `{}`, `[]`} {
		m, _ := laneControlCommitFixture(t)
		args := `{"lane":"codex","expected_revision":0,"after_current":` + flag + `}`
		v := laneMCPStructured(t, laneMCPExchange(t, m, laneMCPRequest("nuntius_lane_pause", args))[0], true)
		if v["code"] != "invalid_request" {
			t.Fatal("after_current coercion accepted")
		}
	}
	for _, args := range []string{`{"lane":"codex","expected_revision":0,"after_current":false}`, `{"lane":"codex","expected_revision":0,"after_current":true,"after_current":false}`} {
		m, _ := laneControlCommitFixture(t)
		name := "nuntius_lane_resume"
		if strings.Count(args, "after_current") == 2 {
			name = "nuntius_lane_pause"
		}
		if v := laneMCPStructured(t, laneMCPExchange(t, m, laneMCPRequest(name, args))[0], true); v["code"] != "invalid_request" {
			t.Fatal("resume flag or duplicate flag accepted")
		}
	}
}

func TestLaneControlMCPCASAuditAndStatus(t *testing.T) {
	for _, flag := range []string{"", `,"after_current":false`, `,"after_current":true`} {
		t.Run(flag, func(t *testing.T) {
			m, store := laneControlCommitFixture(t)
			requests := []string{
				laneMCPRequest("nuntius_lane_pause", `{"lane":"codex","expected_revision":0`+flag+`}`),
				laneMCPRequest("nuntius_lane_pause", `{"lane":"codex","expected_revision":1}`),
				laneMCPRequest("nuntius_lane_pause", `{"lane":"codex","expected_revision":0}`),
				laneMCPRequest("nuntius_lane_resume", `{"lane":"codex","expected_revision":1}`),
				laneMCPRequest("nuntius_lane_pause", `{"lane":"codex","expected_revision":2}`),
				laneMCPRequest("nuntius_lane_pause", `{"lane":"codex","expected_revision":1}`),
				laneMCPRequest("nuntius_status", `{}`),
			}
			responses := laneMCPExchange(t, m, requests...)
			codes := []string{"committed", "unchanged", "revision_conflict", "committed", "committed", "revision_conflict"}
			revisions := []float64{1, 1, 1, 2, 3, 3}
			for i, code := range codes {
				v := laneMCPStructured(t, responses[i], code == "revision_conflict")
				operation := "pause"
				if i == 3 {
					operation = "resume"
					if !strings.Contains(v["explanation"].(string), "started no work") {
						t.Fatal("resume misstates launch behavior")
					}
				}
				if v["code"] != code || v["operation"] != operation || v["revision"] != revisions[i] || v["changed"] != (code == "committed") {
					t.Fatal("CAS result differs from durable operation")
				}
				control := v["lane_control"].(map[string]any)
				if control["revision"] != revisions[i] || control["lane_control_version"] != float64(1) {
					t.Fatal("current safe snapshot absent")
				}
			}
			status := laneMCPResult(t, responses[6], false)
			if status["structuredContent"] != nil {
				t.Fatal("legacy status shape changed")
			}
			var view map[string]any
			if json.Unmarshal([]byte(status["content"].([]any)[0].(map[string]any)["text"].(string)), &view) != nil || view["lane_control"].(map[string]any)["revision"] != float64(3) {
				t.Fatal("status does not supply control list")
			}
			b, err := os.ReadFile(filepath.Join(store, "audit.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			intents, outcomes, conflicts := 0, 0, 0
			origins := map[string]int{}
			for _, line := range bytes.Split(bytes.TrimSpace(b), []byte{'\n'}) {
				var row map[string]any
				if json.Unmarshal(line, &row) != nil {
					t.Fatal("audit JSON invalid")
				}
				if strings.HasPrefix(fmt.Sprint(row["tool"]), "nuntius_lane_") {
					t.Fatal("duplicate generic mutation audit")
				}
				if row["tool"] != "lane_control" && row["tool"] != "lane_pause" && row["tool"] != "lane_resume" {
					continue
				}
				if row["source"] != "mcp" {
					t.Fatal("source is not trusted MCP adapter")
				}
				switch row["result"] {
				case "intent":
					intents++
					if row["after_current"] != (flag == `,"after_current":true` && row["revision"] == float64(1)) {
						t.Fatal("strict after_current provenance lost")
					}
					origins[row["origin"].(string)]++
				case "committed":
					outcomes++
					origins[row["origin"].(string)]++
				case "revision_conflict":
					conflicts++
				default:
					t.Fatal("unexpected manager audit outcome")
				}
			}
			if intents != 3 || outcomes != 3 || conflicts != 2 || len(origins) != 3 {
				t.Fatal("manager audit protocol cardinality")
			}
			for _, count := range origins {
				if count != 2 {
					t.Fatal("intent/outcome correlation differs")
				}
			}
			if runs, err := m.history(); err != nil || len(runs) != 0 {
				t.Fatal("controls or list created a child")
			}
		})
	}
}

func TestLaneControlMCPPausedAndLegacyResults(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	admissionPause(t, m, false)
	if err := m.Freeze("fixture"); err != nil {
		t.Fatal(err)
	}
	before := admissionArtifacts(t, store)
	responses := laneMCPExchange(t, m,
		laneMCPRequest("nuntius_dispatch", `{"lane":"codex","goal_id":"WIRE-PAUSED","text":"fixture"}`),
		laneMCPRequest("nuntius_reply", `{"lane":"codex","text":"fixture"}`),
		laneMCPRequest("nuntius_dispatch", `{"lane":"missing","goal_id":"WIRE-PAUSED","text":"fixture"}`),
		laneMCPRequest("nuntius_signals", `{}`),
		`{"jsonrpc":"2.0","id":2,"method":"not-a-method"}`)
	for _, response := range responses[:2] {
		v := laneMCPStructured(t, response, true)
		if v["code"] != "lane_paused" || v["lane"] != "codex" || v["paused"] != true || v["retryable"] != false || !reflect.DeepEqual(v["blockers"], []any{"lane_paused", "frozen"}) {
			t.Fatal("paused refusal lost primary code or blockers")
		}
		if !strings.Contains(v["explanation"].(string), "All lanes are also frozen") {
			t.Fatal("frozen blocker has no plain reason")
		}
	}
	if !reflect.DeepEqual(before, admissionArtifacts(t, store)) {
		t.Fatal("paused refusals changed run artifacts")
	}
	legacy := laneMCPResult(t, responses[2], true)
	if legacy["structuredContent"] != nil || len(legacy) != 2 {
		t.Fatal("unrelated legacy failure shape changed")
	}
	legacy = laneMCPResult(t, responses[3], false)
	if legacy["structuredContent"] != nil || legacy["content"].([]any)[0].(map[string]any)["text"] != "[]" || len(legacy) != 2 {
		t.Fatal("legacy success shape changed")
	}
	if responses[4]["result"] != nil || responses[4]["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatal("protocol error is no longer JSON-RPC error")
	}
}

func TestLaneControlMCPFailuresAndIsolation(t *testing.T) {
	for _, failure := range []string{"unknown", "corrupt", "audit", "locked", "nil", "startup-config"} {
		t.Run(failure, func(t *testing.T) {
			m, store := laneControlCommitFixture(t)
			lane, want := "codex", "lane_control_unavailable"
			switch failure {
			case "unknown":
				lane, want = "missing", "lane_not_found"
			case "corrupt":
				laneControlPut(t, filepath.Join(store, "lane-control.json"), "{")
			case "audit":
				if err := os.Mkdir(filepath.Join(store, "audit.jsonl"), 0700); err != nil {
					t.Fatal(err)
				}
				want = "audit_unavailable"
			case "locked":
				laneMCPLockedExchange(t, m)
				return
			case "nil":
				m = nil
			case "startup-config":
				m.config.SourcePath = filepath.Join(store, "bad-config.json")
				laneControlPut(t, m.config.SourcePath, "{")
			}
			args := fmt.Sprintf(`{"lane":%q,"expected_revision":0}`, lane)
			v := laneMCPStructured(t, laneMCPExchange(t, m, laneMCPRequest("nuntius_lane_pause", args))[0], true)
			if v["code"] != want || strings.Contains(string(safeJSON(v)), store) {
				t.Fatal("failure not fixed or exposed store path")
			}
			if failure == "startup-config" {
				responses := laneMCPExchange(t, m, laneMCPRequest("nuntius_status", `{}`), laneMCPRequest("nuntius_freeze", `{"reason":"fixture"}`))
				for _, response := range responses {
					laneMCPResult(t, response, true)
				}
				if _, err := os.Stat(filepath.Join(store, "freeze.json")); !os.IsNotExist(err) {
					t.Fatal("broader startup fault bypassed")
				}
			}
		})
	}
}

func laneMCPLockedExchange(t *testing.T, m *Manager) {
	t.Helper()
	in, writer := io.Pipe()
	reader, out := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), m, in, out) }()
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
		_ = in.Close()
		_ = out.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("stdio fixture did not end")
		}
	})
	decoder := json.NewDecoder(reader)
	if _, err := io.WriteString(writer, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if decoder.Decode(&response) != nil {
		t.Fatal("initialize response absent")
	}
	if _, err := io.WriteString(writer, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := io.WriteString(writer, laneMCPRequest("nuntius_lane_pause", `{"lane":"codex","expected_revision":0}`)+"\n"); err != nil {
		t.Fatal(err)
	}
	if decoder.Decode(&response) != nil {
		t.Fatal("locked response absent")
	}
	v := laneMCPStructured(t, response, true)
	if v["code"] != "locked" || v["lane_control"] != nil {
		t.Fatal("bounded lock refusal absent")
	}
}

func TestLaneControlMCPManagerAuditBookkeeping(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	responses := laneMCPExchange(t, m, laneMCPRequest("nuntius_freeze", `{"reason":"fixture"}`), laneMCPRequest("nuntius_unfreeze", `{}`))
	for _, response := range responses {
		laneMCPResult(t, response, false)
	}
	b, err := os.ReadFile(filepath.Join(store, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte{'\n'}) {
		var row map[string]any
		if json.Unmarshal(line, &row) != nil {
			t.Fatal("invalid audit row")
		}
		counts[fmt.Sprint(row["tool"])]++
	}
	if counts["nuntius_freeze"] != 1 || counts["nuntius_unfreeze"] != 1 {
		t.Fatal("manager-audited safety mutation got duplicate generic outcome")
	}
}

func TestLaneControlMCPFaultSafetyAndChildTeardown(t *testing.T) {
	for _, safety := range []string{"nuntius_cancel", "nuntius_freeze"} {
		t.Run(safety, func(t *testing.T) {
			m, store := laneControlCommitFixture(t)
			m.mu.Lock()
			m.Grace = 10 * time.Millisecond
			m.mu.Unlock()
			started := time.Now().UTC()
			if _, err := m.Dispatch("codex", "WIRE-SAFETY", "ignore-int"); err != nil {
				t.Fatal(err)
			}
			// Teardown is verified even if a later assertion aborts the test.
			t.Cleanup(func() {
				if err := m.Cancel("codex"); err != nil {
					r, readErr := m.latest("codex")
					if readErr != nil || r == nil || sameProcess(*r) {
						t.Error("fixture cancellation/teardown failed")
					}
				}
				waitState(t, m, "codex", "ended")
				r, err := m.latest("codex")
				if err != nil || r == nil || r.Ended == nil || sameProcess(*r) {
					t.Error("fixture child end not verified")
				} else {
					t.Logf("offline child start=%s end=%s teardown=verified", started.Format(time.RFC3339Nano), r.Ended.Format(time.RFC3339Nano))
				}
			})
			waitReady(t, m, "codex")
			laneControlPut(t, filepath.Join(store, "lane-control.json"), "{")
			if err := os.WriteFile(filepath.Join(m.config.ReportsDir, "WIRE-REPORT.txt"), []byte("fixture report"), 0600); err != nil {
				t.Fatal(err)
			}
			args := `{"lane":"codex"}`
			if safety == "nuntius_freeze" {
				args = `{"reason":"fixture"}`
			}
			responses := laneMCPExchange(t, m,
				laneMCPRequest("nuntius_status", `{}`), laneMCPRequest("nuntius_tail", `{"lane":"codex"}`),
				laneMCPRequest("nuntius_report", `{"goal_id":"WIRE-REPORT"}`), laneMCPRequest("nuntius_dispatch", `{"lane":"claude","goal_id":"NO-START","text":"fixture"}`),
				laneMCPRequest(safety, args))
			for _, i := range []int{0, 1, 2, 4} {
				laneMCPResult(t, responses[i], false)
			}
			laneMCPResult(t, responses[3], true)
			text := responses[0]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
			if !strings.Contains(text, `"phase":"unavailable"`) || !strings.Contains(text, `"dispatch_eligible":false`) || !strings.Contains(text, `"state":"running"`) {
				t.Fatal("corrupt controls erased current run or enabled starts")
			}
			waitState(t, m, "codex", "ended")
			if runs, err := m.history(); err != nil || len(runs) != 1 {
				t.Fatal("corrupt control started an extra child")
			}
		})
	}
}

func TestLaneControlMCPStartupFaultScope(t *testing.T) {
	for _, name := range []string{"nuntius_status", "nuntius_cancel", "nuntius_freeze", "nuntius_report", "nuntius_tail", "nuntius_lane_pause", "nuntius_lane_resume"} {
		if !mcpStartupAllows(fmt.Errorf("control: %w", errLaneControlUnavailable), name) {
			t.Fatal("isolated admission fault blocked independent tool")
		}
		for _, fault := range []error{errors.New("lane_control_unavailable"), errors.New("run store failure"), errors.New("configuration failure"), &laneControlFault{Code: "audit_unavailable"}} {
			if mcpStartupAllows(fault, name) {
				t.Fatal("unrelated startup fault bypassed by prose or broad type")
			}
		}
	}
	for _, name := range []string{"nuntius_dispatch", "nuntius_reply", "unknown"} {
		if mcpStartupAllows(errLaneControlUnavailable, name) {
			t.Fatal("startup admission fault allowed a start")
		}
	}
}

func TestLaneControlMCPBoundSnapshotAndUncertainty(t *testing.T) {
	m, _ := laneControlCommitFixture(t)
	first, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: true, ExpectedRevision: laneControlRevision(0)}, "mcp")
	if err != nil || first.Snapshot == nil || first.Snapshot.Revision != first.Revision {
		t.Fatal("MCP success lacks bound snapshot")
	}
	_, err = m.setLaneControl(laneControlChange{Lane: "codex", ExpectedRevision: laneControlRevision(1)}, "mcp")
	if err != nil || first.Snapshot.Revision != 1 {
		t.Fatal("prior snapshot changed after another operation")
	}
	for _, stage := range []string{"state.rename", "audit.outcome.append"} {
		r, err := m.setLaneControlIO(laneControlChange{Lane: "codex", Paused: true, ExpectedRevision: laneControlRevision(2)}, "mcp", &laneControlIO{checkpoint: func(at string) error {
			if at == stage {
				return errors.New("fixture failure")
			}
			return nil
		}})
		if err == nil {
			t.Fatal("uncertainty fixture did not fail")
		}
		v := laneControlResponse(m, mcpLaneControlResult{Operation: "pause", Lane: "codex"}, r, err)
		result := mcpToolResult("nuntius_lane_pause", v, err)
		response := map[string]any{"id": float64(2), "result": result}
		// Decode through JSON, exactly as a host sees the structured/text pair.
		var wire map[string]any
		b, _ := json.Marshal(response)
		if json.Unmarshal(b, &wire) != nil {
			t.Fatal("uncertainty result encoding")
		}
		got := laneMCPStructured(t, wire, true)
		if got["code"] != laneControlCode(err) || !strings.Contains(got["explanation"].(string), "do not blindly repeat") {
			t.Fatal("uncertainty lost typed failure or retry guidance")
		}
		// A new isolated fixture keeps each injected crash independent.
		if stage == "state.rename" {
			m, _ = laneControlCommitFixture(t)
			laneControlSet(t, m, true, 0)
			laneControlSet(t, m, false, 1)
		}
	}
}
