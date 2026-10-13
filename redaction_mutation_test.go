package nuntius

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactionUnion(t *testing.T) {
	spans := sourceUnion([]sourceSpan{{10, 20}, {0, 15}, {5, 12}})
	if len(spans) != 1 || spans[0] != (sourceSpan{0, 20}) {
		t.Fatal("PM01 minimum-start union")
	}
}

func TestRedactionCacheContext(t *testing.T) {
	body := proofBody(t, []byte("cache-context"))
	m := &Manager{}
	m.eventWindow("context", proofText(body), false) // public before a new source revision
	stream := append(proofText("---"), proofText(body)...)
	stream = append(stream, proofText("BEGIN PRIVATE KEY")...)
	proofNoBody(t, body, m.eventWindow("context", stream, false).final)
	for _, proof := range m.eventWindows[eventWindowID{"context", false}].lexical {
		proofNoBody(t, body, string(proof.wire))
	}
	// A policy mismatch cannot reuse either lexical or formatted facts.
	window := m.eventWindow("public", proofText("ordinary public"), false)
	window.policy = "obsolete-policy"
	window.final = body
	proofNoBody(t, body, m.eventWindow("public", proofText("ordinary public"), false).final)
	// A cold or evicted reader must inspect an opener outside the display window.
	stream = proofText("-----BE")
	for len(stream) < 2<<20 {
		stream = append(stream, proofText(strings.Repeat("x", 4096))...)
	}
	stream = append(stream, proofText(body)...)
	proofNoBody(t, body, (&Manager{}).eventWindow("evicted", stream, false).final)
}

func TestRedactionTypedCollision(t *testing.T) {
	wire := []byte(`{"type":"text","part/text":"-----BEGIN PRIVATE KEY-----","part":{"type":"text","text":"-----END PRIVATE KEY-----\npublic"}}` + "\n")
	p := approveEvents(wire, false)
	if len(p.events) != 1 || !p.events[0].hidden {
		t.Fatal("J01 typed collision dominance")
	}
}

// Each overlay introduces a single named semantic defect. The original source
// is never edited by this harness. A compile/setup failure cannot kill a mutant.
// All output retained here is IDs, hashes and booleans, never child responses.
func TestRedactionMutationProof(t *testing.T) {
	if testing.Short() {
		t.Fatal("MUTATION short mode is not proof")
	}
	type mutation struct{ id, file, before, after, target string }
	mutants := []mutation{
		{"M14-uninspected-nested-arguments", "redaction_pass.go", "!valid || sourceUnprovedMCPArguments(scalars)", "!valid", "TestIndependentReviewNestedArguments"},
		{"M01-orphan-line", "redaction_pass.go", `if open < 0 || label != marker.label {
				return redactionHidden
			}`, `if open < 0 || label != marker.label {
				return spliceSource(text, []sourceSpan{{marker.start,len(text)}},redactionHidden)
			}`, "TestRedactionPlainOracle"},
		{"M02-minimum-union", "redaction_pass.go", "return spans[i].start < spans[j].start", "return spans[i].start > spans[j].start", "TestRedactionUnion"},
		{"M03-local-field-return", "redaction_pass.go", `if inspect.uncertain || pending && inspect.continuation || len(inspect.markers) > 0 && role == "" {
				uncertain = true
			}`, `if inspect.uncertain || pending && inspect.continuation || len(inspect.markers) > 0 && role == "" {
				return approvedSource{events: []approvedEvent{{wire: line}}}
			}`, "TestRedactionScopesAndOrigins"},
		{"M04-path-collapse", "redaction_pass.go", `part := func(i int, m string) bool`, `if len(s.path)>0 && strings.Contains(s.path[len(s.path)-1].member,"text") {return "opencode-text"}
	part := func(i int, m string) bool`, "TestRedactionTypedCollision"},
		{"M05-complete-loses-delimiter", "redaction_pass.go", "if inspect.pending {", "if inspect.pending && len(inspect.markers)==0 {", "TestRedactionPollingAndCache"},
		{"M06-metadata-releases-pending", "redaction_pass.go", "scalars, valid := sourceScalars(line)", "pending=false; if openLabel==\"\" {held=-1}\n\t\tscalars, valid := sourceScalars(line)", "TestRedactionPollingAndCache"},
		{"M07-polling-seals", "redaction_pass.go", "var events []tentativeEvent", "complete=true\n\tvar events []tentativeEvent", "TestRedactionPollingAndCache"},
		{"M08-raw-terminal-final", "terminal_turn.go", "return true, window.terminalFinal", `var raw struct {Part struct {Text string}}; for _,line:=range bytes.Split(snapshot,[]byte("\n")){var v struct {Type string; Part struct {Text string}}; _ = json.Unmarshal(line,&v); if v.Type=="text"{raw.Part.Text=v.Part.Text}}; return true,raw.Part.Text`, "TestRedactionTerminalBalancedReference"},
		{"M08b-raw-reference-fallback", "terminal_turn.go", "if finalIndex != window.terminalIndex {\n\t\treturn true, redactionHidden", `if finalIndex != window.terminalIndex {var raw struct {Part struct {Text string}}; for _,line:=range bytes.Split(snapshot,[]byte("\n")){var v struct {Type string; Part struct {Text string}}; _ = json.Unmarshal(line,&v); if v.Type=="text"{raw.Part.Text=v.Part.Text}}; return true,raw.Part.Text`, "TestRedactionTerminalCP02"},
		{"M09-stale-body-cache", "event_cache.go", `w.final = "[REDACTED]"`, `w.final = "[REDACTED]"; if old!=nil {w.final=old.final}`, "TestRedactionCacheContext"},
		{"M10-eviction-tail-reset", "event_cache.go", "projection := approveEventsWithCache(b, false, previous)", `scan:=b; if len(scan)>1<<20 {scan=scan[len(scan)-(1<<20):];if i:=strings.IndexByte(string(scan),'\n');i>=0{scan=scan[i+1:]}}; projection := approveEventsWithCache(scan,false,previous)`, "TestRedactionCacheContext"},
		{"M11-duplicate-collapse", "redaction_pass.go", "if seen[key] {", "if false && seen[key] {", "TestRedactionScopesAndOrigins"},
		{"M11b-skip-member-names", "redaction_pass.go", "for _, s := range scalars {\n\t\t\tinspect := inspectSourceText(s.text)", "for _, s := range scalars { if s.key {continue}\n\t\t\tinspect := inspectSourceText(s.text)", "TestRedactionScopesAndOrigins"},
		{"M11c-split-origin-unit", "redaction_pass.go", "sourceSpan{a, p.i}", "sourceSpan{a, a+1}", "TestRedactionScopesAndOrigins"},
		{"M12-clip-before-approval", "events.go", "if len(b) > 65536 {\n\t\treturn redactionHidden, nil", "if len(b) > 65536 {\n\t\treturn bounded(string(b[:65536]),65536),nil", "TestRedactionPublicSinks"},
		{"M13-untrusted-type-value", "redaction_pass.go", "!sourceStructuralLiteral(s, scalars)", `(len(s.path)==0 || s.path[len(s.path)-1].member!="type")`, "TestRedactionOpenMetadataType"},
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal("MUTATION root")
	}
	for _, m := range mutants {
		t.Run(m.id, func(t *testing.T) {
			originalPath := filepath.Join(root, m.file)
			original, err := os.ReadFile(originalPath)
			if err != nil {
				t.Fatal("MUTATION source")
			}
			if bytes.Count(original, []byte(m.before)) != 1 {
				t.Fatal("MUTATION site identity")
			}
			changed := bytes.Replace(original, []byte(m.before), []byte(m.after), 1)
			if strings.HasPrefix(m.id, "M08") {
				changed = bytes.Replace(changed, []byte("import ("), []byte("import (\n\"bytes\""), 1)
			}
			dir := t.TempDir()
			mutated := filepath.Join(dir, "mutated.go")
			overlay := filepath.Join(dir, "overlay.json")
			mapping, _ := json.Marshal(map[string]any{"Replace": map[string]string{originalPath: mutated}})
			if os.WriteFile(mutated, changed, 0600) != nil || os.WriteFile(overlay, mapping, 0600) != nil {
				t.Fatal("MUTATION overlay")
			}
			cmd := exec.Command("go", "test", "-overlay", overlay, ".", "-run", "^"+m.target+"$", "-count=1")
			cmd.Dir = root
			output, err := cmd.CombinedOutput()
			red := err != nil && bytes.Contains(output, []byte("--- FAIL: "+m.target)) && !bytes.Contains(output, []byte("[build failed]"))
			if !red {
				t.Fatalf("%s semantic-red=false", m.id)
			}
			current, e := os.ReadFile(originalPath)
			if e != nil || !bytes.Equal(original, current) {
				t.Fatal("MUTATION original identity changed")
			}
			green := exec.Command("go", "test", ".", "-run", "^"+m.target+"$", "-count=1")
			green.Dir = root
			if _, err := green.CombinedOutput(); err != nil {
				t.Fatalf("%s restored-green=false", m.id)
			}
			t.Logf("%s semantic-red=true restored-green=true original-sha256=%x", m.id, sha256.Sum256(original))
		})
	}
}
