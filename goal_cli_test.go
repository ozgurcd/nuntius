package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func goalCLIConfig(t *testing.T, c Config) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "lanes.json")
	b, _ := json.Marshal(c)
	if os.WriteFile(p, b, 0600) != nil {
		t.Fatal("write config fixture")
	}
	return p
}

func goalCLICall(t *testing.T, config, store, input string, args ...string) (int, goalCLIResult) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	code := CLI(context.Background(), append([]string{"--config", config, "--store", store}, args...), strings.NewReader(input), &out, &diagnostic)
	var v goalCLIResult
	if json.Unmarshal(out.Bytes(), &v) != nil {
		t.Fatal("invalid CLI JSON")
	}
	return code, v
}

func goalCLIProcess(t *testing.T, config, store, input string, args ...string) (int, map[string]any) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "nuntius")
	build := exec.Command("go", "build", "-o", binary, "./cmd/nuntius")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI failed: %s", b)
	}
	cmd := exec.Command(binary, append([]string{"--config", config, "--store", store}, args...)...)
	cmd.Stdin = strings.NewReader(input)
	var out, diagnostic bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diagnostic
	code := 0
	if err := cmd.Run(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else {
			t.Fatal("CLI failed to execute")
		}
	}
	var result map[string]any
	if json.Unmarshal(out.Bytes(), &result) != nil {
		t.Fatalf("CLI did not return a JSON result; exit=%d", code)
	}
	return code, result
}

func TestGoalCLIRecords(t *testing.T) {
	c, store := fixture(t)
	config := goalCLIConfig(t, c)
	input := `{"version":1,"id":"CLI-GOAL","title":"CLI goal","size":"M","lane_kind":"codex","kinds":["codex"],"work_kind":"code","lane_binding":"preferred","order":1,"after":[],"writes":[],"resources":[]}`
	code, first := goalCLIProcess(t, config, store, input, "goal", "create", "CLI-GOAL", "--op", "create-1", "--stdin", "--json")
	if code != 0 || first["result"] != "committed" {
		t.Fatal("typed CLI creation did not commit")
	}
	before := laneCLITree(t, store)
	code, again := goalCLIProcess(t, config, store, input, "goal", "create", "CLI-GOAL", "--op", "create-1", "--stdin", "--json")
	if code != 0 || !reflect.DeepEqual(first, again) || !reflect.DeepEqual(before, laneCLITree(t, store)) {
		t.Fatal("create retry changed receipt or store")
	}
	code, view := goalCLIProcess(t, config, store, "", "goal", "show", "CLI-GOAL", "--json")
	if code != 0 {
		t.Fatal("goal show failed")
	}
	data := view["data"].(map[string]any)
	if _, ok := data["goal"].(map[string]any)["after"].([]any); !ok {
		t.Fatal("dependencies were not a list")
	}
	if !reflect.DeepEqual(before, laneCLITree(t, store)) {
		t.Fatal("show changed native store")
	}
	code, conflict := goalCLIProcess(t, config, store, input, "goal", "update", "CLI-GOAL", "--revision", "9", "--op", "bad-rev", "--stdin", "--json")
	if code != 3 || conflict["expected_revision"] != float64(9) || conflict["found_revision"] != float64(1) {
		t.Fatal("CAS refusal omitted expected/found revisions")
	}
	code, _ = goalCLIProcess(t, config, store, "", "goal", "cancel", "CLI-GOAL", "--revision", "1", "--op", "cancel-1", "--reason", "No longer needed", "--authority", "judge", "--evidence", "review", "--json")
	if code != 0 {
		t.Fatal("goal cancellation failed")
	}
	if _, err := os.Stat(filepath.Join(store, "runs")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(store, "runs"))
		if len(entries) != 0 {
			t.Fatal("goal cancellation launched a child")
		}
	}
}

func TestGoalCLIInvalidPassive(t *testing.T) {
	c, store := fixture(t)
	config := goalCLIConfig(t, c)
	for _, args := range [][]string{
		{"goal", "set", "CLI-GOAL", "--json"},
		{"goal", "create", "CLI-GOAL", "--op", "x", "--stdin", "--revision", "1", "--json"},
		{"goal", "show", "CLI-GOAL", "extra", "--json"},
		{"goal", "show", "CLI-GOAL", "--json", "--json"},
		{"goal", "ended", "CLI-GOAL", "--run", "bad/run", "--json"},
		{"dispatch", "CLI-GOAL", "--lane", "codex", "--op", "x", "--revision", "0", "--json"},
	} {
		before := laneCLITree(t, store)
		code, result := goalCLIProcess(t, config, store, "", args...)
		if code != 2 || result["result"] != "invalid_request" || !reflect.DeepEqual(before, laneCLITree(t, store)) {
			t.Fatal("invalid syntax mutated or did not exit 2")
		}
	}
	base := `{"version":1,"id":"CLI-GOAL","title":"CLI goal","size":"M","lane_kind":"codex","kinds":["codex"],"work_kind":"code","lane_binding":"preferred","order":1,"after":[],"writes":[],"resources":[]}`
	for _, input := range []string{
		strings.Replace(base, `"after":[]`, `"after":null`, 1),
		strings.Replace(base, `"after":[]`, `"after":"CLI-OTHER"`, 1),
		strings.Replace(base, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(base, `"title":"CLI goal"`, `"title":"CLI goal","unknown":true`, 1),
		base + `{}`, strings.Repeat(" ", goalSealMaxBytes+1),
	} {
		before := laneCLITree(t, store)
		code, v := goalCLICall(t, config, store, input, "goal", "create", "CLI-GOAL", "--op", "invalid-input", "--stdin", "--json")
		if code != 2 || v.Result != "invalid_request" || !reflect.DeepEqual(before, laneCLITree(t, store)) {
			t.Fatal("invalid spec wrote store")
		}
	}
}

func TestGoalCLISealDependencies(t *testing.T) {
	c, store := fixture(t)
	config := goalCLIConfig(t, c)
	spec, in := briefFixture(t)
	spec.ID = "CLI-FIRST"
	spec.After = []string{"CLI-SECOND"}
	spec.Writes = []string{}
	spec.Resources = []string{}
	input, _ := json.Marshal(spec)
	if code, _ := goalCLICall(t, config, store, string(input), "goal", "create", spec.ID, "--op", "first", "--stdin", "--json"); code != 0 {
		t.Fatal("first creation")
	}
	spec.ID = "CLI-SECOND"
	spec.After = []string{}
	input, _ = json.Marshal(spec)
	if code, _ := goalCLICall(t, config, store, string(input), "goal", "create", spec.ID, "--op", "second", "--stdin", "--json"); code != 0 {
		t.Fatal("second creation")
	}
	spec.After = []string{"CLI-FIRST"}
	input, _ = json.Marshal(spec)
	before := laneCLITree(t, store)
	code, refusal := goalCLICall(t, config, store, string(input), "goal", "update", spec.ID, "--revision", "1", "--op", "cycle", "--stdin", "--json")
	if code != 3 || !reflect.DeepEqual(refusal.Cycle, []string{"CLI-FIRST", "CLI-SECOND", "CLI-FIRST"}) || !reflect.DeepEqual(before, laneCLITree(t, store)) {
		t.Fatal("cycle path or nonmutation")
	}
	// Remove and then add a dependency through the same typed update surface.
	spec.ID = "CLI-FIRST"
	spec.After = []string{}
	input, _ = json.Marshal(spec)
	if code, _ := goalCLICall(t, config, store, string(input), "goal", "update", spec.ID, "--revision", "1", "--op", "remove-dependency", "--stdin", "--json"); code != 0 {
		t.Fatal("dependency removal")
	}
	spec.After = []string{"CLI-SECOND"}
	input, _ = json.Marshal(spec)
	if code, _ := goalCLICall(t, config, store, string(input), "goal", "update", spec.ID, "--revision", "2", "--op", "add-dependency", "--stdin", "--json"); code != 0 {
		t.Fatal("dependency addition")
	}
	if code, _ := goalCLICall(t, config, store, "", "goal", "approve", spec.ID, "--revision", "3", "--op", "unsealed", "--reason", "Reviewed", "--authority", in.Authority, "--evidence", "review", "--json"); code != 3 {
		t.Fatal("unsealed approval")
	}
	in.Body = []byte("GOAL: CLI-FIRST (size " + string(spec.Size) + ")\nKeep the exact brief.\n")
	payload, _ := json.Marshal(map[string]any{"body": string(in.Body), "standards": string(in.Standards), "profile": in.Profile, "authority": in.Authority, "source_state": in.SourceState, "reviewed_source_state": in.ReviewedSourceState})
	if code, _ := goalCLICall(t, config, store, string(payload), "goal", "seal", spec.ID, "--revision", "3", "--op", "seal", "--stdin", "--json"); code != 0 {
		t.Fatal("seal")
	}
	if code, _ := goalCLICall(t, config, store, "", "goal", "approve", spec.ID, "--revision", "4", "--op", "approve", "--reason", "Reviewed", "--authority", in.Authority, "--evidence", "review", "--source-state", in.SourceState, "--json"); code != 0 {
		t.Fatal("sealed approval")
	}
	code, refusal = goalCLICall(t, config, store, "", "dispatch", spec.ID, "--revision", "5", "--op", "launch", "--lane", "codex", "--json")
	if code != 3 || refusal.Code != "trusted_dispatch_context_unavailable" {
		t.Fatal("missing provider was inferred from argv")
	}
	if code, _ := goalCLICall(t, config, store, "", "goal", "cancel", "CLI-SECOND", "--revision", "1", "--op", "cancel-second", "--reason", "Cancelled", "--authority", "judge", "--evidence", "decision", "--json"); code != 0 {
		t.Fatal("cancel dependency")
	}
	code, view := goalCLICall(t, config, store, "", "goal", "show", "CLI-FIRST", "--json")
	if code != 0 {
		t.Fatal("show")
	}
	encoded, _ := json.Marshal(view.Data)
	if !bytes.Contains(encoded, []byte("blocked: CLI-SECOND was cancelled")) || !bytes.Contains(encoded, []byte("depends_on")) {
		t.Fatal("cancelled dependency blocker")
	}
	if code, _ := goalCLICall(t, config, store, "", "goal", "cover", "CLI-FIRST", "--revision", "5", "--op", "cover", "--by", "CLI-SECOND", "--reason", "Covered scope", "--json"); code != 0 {
		t.Fatal("coverage hold")
	}
	if code, _ := goalCLICall(t, config, store, "", "goal", "note", "CLI-FIRST", "--revision", "6", "--op", "note", "--text", "Review observation", "--evidence", "note-record", "--json"); code != 0 {
		t.Fatal("note")
	}
}

func TestGoalCLILoop(t *testing.T) {
	m, run := goalEndSavedFixture(t)
	config := goalCLIConfig(t, m.config)
	// The CLI observes the real full run identity; it cannot assign an outcome.
	code, _ := goalCLIProcess(t, config, m.store, "", "goal", "ended", run.GoalID, "--run", run.ID, "--json")
	if code != 0 {
		t.Fatal("full native run ID refused")
	}
	before := laneCLITree(t, m.store)
	code, v := goalCLIProcess(t, config, m.store, "", "goal", "ended", run.GoalID, "--run", run.ID, "--json")
	if code != 0 || v["result"] != "no_op" || !reflect.DeepEqual(before, laneCLITree(t, m.store)) {
		t.Fatal("repeated end was not a no-op")
	}
	code, refusal := goalCLICall(t, config, m.store, "", "goal", "ended", run.GoalID, "--run", strings.Repeat("f", 32), "--json")
	if code != 3 || refusal.Code != "attempt_mismatch" || !reflect.DeepEqual(before, laneCLITree(t, m.store)) {
		t.Fatal("old run end accepted or changed store")
	}
	p, _, err := readGoalRecords(m.store)
	if err != nil {
		t.Fatal("read end")
	}
	revision := strconv.FormatUint(p.records[run.GoalID].revision, 10)
	args := []string{"goal", "verify", run.GoalID, "--revision", revision, "--op", "verified", "--reason", "Reviewed work", "--authority", "judge", "--evidence", "test-review", "--attempt", run.ID, "--alternate-evidence", "--json"}
	code, _ = goalCLIProcess(t, config, m.store, "", args...)
	if code != 0 {
		t.Fatal("verify ended attempt")
	}
	before = laneCLITree(t, m.store)
	code, v = goalCLIProcess(t, config, m.store, "", args...)
	if code != 0 || v["result"] != "no_op" || !reflect.DeepEqual(before, laneCLITree(t, m.store)) {
		t.Fatal("verify duplicate wrote history")
	}
	wrong := append([]string{}, args...)
	for i := range wrong {
		if wrong[i] == "verified" {
			wrong[i] = "dispatch-create"
		}
	}
	code, refusal = goalCLICall(t, config, m.store, "", wrong...)
	if code != 3 || refusal.Code != "operation_id_reused" || !reflect.DeepEqual(before, laneCLITree(t, m.store)) {
		t.Fatal("terminal no-op bypassed operation identity")
	}
	args[len(args)-2] = "--report-digest" // malformed variant must not bypass verification
	code, _ = goalCLIProcess(t, config, m.store, "", args...)
	if code != 2 {
		t.Fatal("malformed verification accepted")
	}
}

func TestGoalCLIExactDispatch(t *testing.T) {
	m, req, ctx := dispatchFixture(t)
	r := goalCLIRequest{action: "dispatch", id: req.GoalID, op: req.Key, lane: req.Lane, revision: req.ExpectedRevision}
	trusted := func(goalDispatchRequest) (goalDispatchContext, error) { return ctx, nil }
	v := m.goalCLI(r, trusted)
	if v.Result != "committed" {
		t.Fatal("exact sealed delegation failed")
	}
	again := m.goalCLI(r, trusted)
	if again.Result != "committed" {
		t.Fatal("dispatch retry failed")
	}
	active, err := m.latest(req.Lane)
	if err != nil {
		t.Fatal("live dispatch fixture")
	}
	old, _ := dispatchRecord(t, m, req.GoalID)
	config := goalCLIConfig(t, m.config)
	code, veto := goalCLICall(t, config, m.store, "", "goal", "cancel", req.GoalID, "--op", "cancel-live", "--revision", strconv.FormatUint(old.revision, 10), "--reason", "Review the live run", "--authority", "judge", "--evidence", "review", "--json")
	current, err := m.latest(req.Lane)
	if code != 3 || veto.Code != "attempt_unsettled" || err != nil || current.ID != active.ID || current.StopAt != nil || !sameProcess(*active) {
		t.Fatal("goal cancellation changed live process")
	}
	// A stale request and a second operation cannot manufacture another child.
	r.op = "different"
	r.revision = 1
	if v = m.goalCLI(r, trusted); v.Result != "refused" || v.Code != "revision_conflict" {
		t.Fatal("stale dispatch accepted")
	}
	dispatchStarts(t, m, 1)
}

func TestGoalCLIRetryLater(t *testing.T) {
	for _, action := range []string{"seal", "cover"} {
		t.Run(action, func(t *testing.T) {
			c, store := fixture(t)
			config := goalCLIConfig(t, c)
			spec, in := briefFixture(t)
			spec.ID = "CLI-RETRY"
			spec.After = []string{}
			spec.Writes = []string{}
			spec.Resources = []string{}
			input, _ := json.Marshal(spec)
			if code, _ := goalCLICall(t, config, store, string(input), "goal", "create", spec.ID, "--op", "create-retry", "--stdin", "--json"); code != 0 {
				t.Fatal("create retry fixture")
			}
			args := []string{"goal", action, spec.ID, "--revision", "1", "--op", "original", "--json"}
			payload := ""
			if action == "cover" {
				args = append(args, "--by", "CLI-TARGET", "--reason", "Covered scope")
				spec.Coverage = &nativeCoverageHold{CoveredBy: "CLI-TARGET", Reason: "Covered scope"}
			} else {
				in.Body = []byte("GOAL: CLI-RETRY (size " + string(spec.Size) + ")\nExact brief.\n")
				b, _ := json.Marshal(map[string]any{"body": string(in.Body), "standards": string(in.Standards), "profile": in.Profile, "authority": in.Authority, "source_state": in.SourceState, "reviewed_source_state": in.ReviewedSourceState})
				payload = string(b)
				args = append(args, "--stdin")
			}
			code, first := goalCLICall(t, config, store, payload, args...)
			if code != 0 {
				t.Fatal("original command")
			}
			spec.Title = "Later title"
			input, _ = json.Marshal(spec)
			if code, _ := goalCLICall(t, config, store, string(input), "goal", "update", spec.ID, "--revision", "2", "--op", "later", "--stdin", "--json"); code != 0 {
				t.Fatal("later update")
			}
			before := laneCLITree(t, store)
			code, again := goalCLICall(t, config, store, payload, args...)
			if code != 0 || !reflect.DeepEqual(first, again) || !reflect.DeepEqual(before, laneCLITree(t, store)) {
				t.Fatal("delayed retry lost original receipt or rewrote history")
			}
		})
	}
}

func TestGoalCLIResume(t *testing.T) {
	m, parent, h, _ := linkedHeld(t)
	linkedControl(t, m, false)
	id, err := m.sendHeld(h, nil)
	if err != nil {
		t.Fatal("start owned linked fake")
	}
	waitReady(t, m, parent.Lane)
	child := successorRun(t, m, id)
	if child.ID == parent.ID || child.ParentRunID != parent.ID {
		t.Fatal("not a genuine resume chain")
	}
	if err = m.Cancel(parent.Lane); err != nil {
		t.Fatal("stop owned fixture")
	}
	waitState(t, m, parent.Lane, "ended")
	if !goalAttemptDefinitelyGone(*child) {
		t.Fatal("child still live")
	}
	if err = m.Close(); err != nil {
		t.Fatal("close fixture")
	}
	config := goalCLIConfig(t, m.config)
	before := laneCLITree(t, m.store)
	code, v := goalCLIProcess(t, config, m.store, "", "goal", "ended", parent.GoalID, "--run", child.ID, "--json")
	if code != 0 || v["result"] != "no_op" || !reflect.DeepEqual(before, laneCLITree(t, m.store)) {
		t.Fatal("actual resumed-run end was not accepted idempotently")
	}
	code, refusal := goalCLICall(t, config, m.store, "", "goal", "ended", parent.GoalID, "--run", parent.ID, "--json")
	if code != 3 || refusal.Code != "attempt_mismatch" || !reflect.DeepEqual(before, laneCLITree(t, m.store)) {
		t.Fatal("late parent end overwrote latest resumed run")
	}
}

func TestGoalCLIReadSafety(t *testing.T) {
	c, store := fixture(t)
	config := goalCLIConfig(t, c)
	spec, _ := briefFixture(t)
	spec.ID = "CLI-SAFE"
	spec.Title = "Visit https://example.test/reference"
	spec.After = []string{}
	spec.Writes = []string{}
	spec.Resources = []string{}
	input, _ := json.Marshal(spec)
	if code, _ := goalCLICall(t, config, store, string(input), "goal", "create", spec.ID, "--op", "safe", "--stdin", "--json"); code != 0 {
		t.Fatal("safe fixture")
	}
	before := laneCLITree(t, store)
	for _, args := range [][]string{{"goal", "show", spec.ID, "--json"}, {"goal", "queue", "--state", "drafted", "--search", "SAFE", "--json"}, {"goal", "audit", "--json"}} {
		code, v := goalCLICall(t, config, store, "", args...)
		b, _ := json.Marshal(v)
		if (code != 0 && !(args[1] == "queue" && code == 4)) || bytes.Contains(b, []byte("https://")) || !reflect.DeepEqual(before, laneCLITree(t, store)) {
			t.Fatal("read safety/passivity")
		}
		if args[1] == "queue" {
			data := v.Data.(map[string]any)
			if data["matched"] != float64(1) {
				t.Fatal("native picker filter")
			}
		}
	}
	code, v := goalCLICall(t, config, store, "", "goal", "queue", "--search", "ABSENT", "--json")
	if code != 4 || v.Data.(map[string]any)["matched"] != float64(0) {
		t.Fatal("empty native picker result")
	}
	var human, diagnostic bytes.Buffer
	if code := CLI(context.Background(), []string{"--config", config, "--store", store, "goal", "show", spec.ID}, strings.NewReader(""), &human, &diagnostic); code != 0 || strings.Contains(human.String(), "https://") || !strings.HasPrefix(human.String(), "ok\n") {
		t.Fatal("human JSON policy parity")
	}
	var out bytes.Buffer
	code = writeGoalCLI(&out, &diagnostic, true, goalCLIResult{Result: "ok", Data: strings.Repeat("x", queueMaxBytes+1)})
	var oversized goalCLIResult
	if code != 4 || json.Unmarshal(out.Bytes(), &oversized) != nil || oversized.Code != "response_too_large" {
		t.Fatal("oversized result looked complete or malformed")
	}
}
