package nuntius

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRereviewOrphanAfterOtherMarkers(t *testing.T) {
	full, body := reviewKey(t)
	cases := []struct{ id, wire string }{
		{"AF01", fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"metadata":%q,"ending":"-----END PRIVATE KEY-----"}`, body, full)},
		{"AF02", fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"metadata":"-----BE","ending":"-----END PRIVATE KEY-----"}`, body)},
		{"AF03", fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"metadata":"-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----","ending":"-----END PRIVATE KEY-----"}`, body)},
		{"AF04", fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"metadata":"-----BEGIN PRIVATE KEY-----","ending":"-----END RSA PRIVATE KEY-----"}`, body)},
		{"AF05", fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"metadata":"-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----\n-----END PRIVATE KEY-----"}`, body)},
		{"AF06", fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"metadata":"-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----","orphan":"-----END PRIVATE KEY-----"}`, body)},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			p := privateEventPrivacy{}
			safe, _ := p.project([]byte(tc.wire))
			exposed := strings.Contains(string(safe), body)
			t.Logf("%s/projection exposed=%t orphan=%t", tc.id, exposed, p.state.orphan)
			if exposed {
				t.Error("same-event orphan hide-back=false")
			}
			hotfixReviewSinks(t, []byte(tc.wire+"\n"), "", body)
			if tc.id == "AF03" || tc.id == "AF06" {
				for sink, exposed := range reviewSinks(t, tc.wire, body, []byte(tc.wire+"\n")) {
					t.Logf("%s/%s exposed=%t", tc.id, sink, exposed)
					if exposed {
						t.Errorf("%s/%s no-key=false", tc.id, sink)
					}
				}
			}
		})
	}
}

func TestRereviewPlainHideBackMerge(t *testing.T) {
	full, body := reviewKey(t)
	input := "public-before\n" + body + "\n" + full + "public-between\n-----END PRIVATE KEY-----\n" + full
	got := Redact(input)
	t.Logf("PM01/plain exposed=%t prefix-visible=%t", strings.Contains(got, body), strings.Contains(got, "public-before"))
	if got != privateHidden {
		t.Error("plain whole-segment hide-back=false")
	}
	for sink, exposed := range reviewSinks(t, input, body) {
		t.Logf("PM01/%s exposed=%t", sink, exposed)
		if exposed {
			t.Errorf("PM01/%s no-key=false", sink)
		}
	}
}

func TestRereviewFieldAndEventSplits(t *testing.T) {
	_, body := reviewKey(t)
	cut := len(body) / 2
	a, b := body[:cut], body[cut:]
	// Compare each contributing piece. A concatenation-only check would miss a field leak.
	wire := []byte(fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"earlier":%q,"ending":"-----END PRIVATE KEY-----"}`+"\n", a, b))
	p := privateEventPrivacy{}
	safe, _ := p.project(wire)
	t.Logf("FS01/direct first-piece exposed=%t second-piece exposed=%t orphan=%t", strings.Contains(string(safe), a), strings.Contains(string(safe), b), p.state.orphan)
	hotfixReviewSinks(t, wire, "", a)
	hotfixReviewSinks(t, wire, "", b)
	// The earlier complete event is outside hide-back; the final piece must remain hidden.
	events := append(privateMessage(a), privateMessage(b+"\n-----END PRIVATE KEY-----")...)
	w := (&Manager{}).eventWindow("split", events, false)
	text := w.final + strings.Join(w.tail, "\n")
	if !strings.Contains(text, a) || strings.Contains(text, b) || strings.Contains(text, body) {
		t.Error("event-scope split=false")
	}
	// Pending context, unlike an ordinary earlier event, is rewound across events.
	events = append(privateMessage("--"), privateMessage(body)...)
	events = append(events, privateMessage("END PRIVATE KEY")...)
	hotfixReviewSinks(t, events, "", body)
}

func TestRereviewMemberPathsAndBudgets(t *testing.T) {
	_, body := reviewKey(t)
	for i, wire := range []string{
		`{"type":"text","part\/text":"-----BEGIN PRIVATE KEY-----","part":{"text":"-----END PRIVATE KEY-----"}}`,
		`{"type":"text","part\u002ftext":"-----BEGIN PRIVATE KEY-----","part":{"text":"-----END PRIVATE KEY-----"}}`,
		`{"type":"assistant","message":{"content":{"[]":{"text":"-----BEGIN PRIVATE KEY-----"}}},"message/content/[]/text":"-----END PRIVATE KEY-----"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","input/command":"-----BEGIN PRIVATE KEY-----","input":{"command":"-----END PRIVATE KEY-----"}}]}}`,
		`{"type":"text","part":{"text":"-----END PRIVATE KEY-----"},"part/text":"-----BEGIN PRIVATE KEY-----"}`,
		fmt.Sprintf(`{"type":"text",%q:{"part":{"text":"-----BEGIN PRIVATE KEY-----"}},"part":{"text":"-----END PRIVATE KEY-----"}}`, strings.Repeat("\\", 40)),
		fmt.Sprintf(`{"type":"text",%q:"-----BEGIN PRIVATE KEY-----","part":{"text":"-----END PRIVATE KEY-----"}}`, strings.Repeat("x", 128)),
		fmt.Sprintf(`{"type":"text","part":{"text":%q}}`, strings.Repeat("x", (1<<20)+1)+"-----END PRIVATE KEY-----"),
	} {
		t.Run(fmt.Sprintf("JP%02d", i), func(t *testing.T) {
			p := privateEventPrivacy{}
			safe, _ := p.project([]byte(wire))
			if !p.state.ambiguous {
				t.Error("untrusted-pair ambiguity=false")
			}
			if strings.Contains(string(safe), body) {
				t.Error("projection no-key=false")
			}
			hotfixReviewSinks(t, append([]byte(wire+"\n"), privateMessage(body)...), "", body)
		})
	}
	// Ordered actual content arrays remain a trusted stream; preserve public bytes exactly.
	wire := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"-----BEGIN PRIVATE KEY-----"},{"type":"text","text":"fixture"},{"type":"text","text":"-----END PRIVATE KEY-----"},{"type":"text","text":"public-after\tβ"}]}}`)
	p := privateEventPrivacy{}
	safe, _ := p.project(wire)
	if p.state.ambiguous || !strings.Contains(string(safe), `"public-after\tβ"`) {
		t.Error("trusted-array preservation=false")
	}
}

func TestRereviewTerminalPendingVariants(t *testing.T) {
	_, body := reviewKey(t)
	for i, continuation := range []string{
		`{"type":"tool_use","part":{"type":"tool","metadata":"END PRIVATE KEY"}}` + "\n" + terminalStop,
		`{"type":"step_finish","part":{"type":"step-finish","reason":"stop","metadata":"BEGIN PRIVATE KEY"}}` + "\n",
		terminalText("BEGIN PRIVATE KEY") + terminalStop,
		`{"type":"tool_use","part":{"type":"tool","metadata":"BEGIN PRIVATE KEY"}}` + "\n" + terminalStop,
	} {
		t.Run(fmt.Sprintf("TP%02d", i), func(t *testing.T) {
			c, store := opencodeFixture(t)
			m, e := NewManager(c, store)
			if e != nil {
				t.Fatal("fixture")
			}
			defer m.Close()
			events := terminalStart + terminalText("--") + terminalText(body) + continuation
			r := seedTerminalRun(t, m, events, "ended")
			ended, final := m.terminalTurn(r)
			if !ended {
				t.Fatal("terminal control=false")
			}
			if strings.Contains(final, body) {
				t.Errorf("TP%02d/final exposed=true", i)
			}
			rows, e := m.Tail(r.Lane, 200)
			if e != nil {
				t.Fatal("tail entrance")
			}
			if strings.Contains(strings.Join(rows, "\n"), body) {
				t.Errorf("TP%02d/tail exposed=true", i)
			}
			// The private state must not relax raw terminal stop validation.
			invalid := strings.Replace(events, `"reason":"stop"`, `"reason":"continue"`, 1)
			if os.WriteFile(filepath.Join(store, "runs", r.ID, "events.jsonl"), []byte(invalid), 0600) != nil {
				t.Fatal("fixture write")
			}
			if ended, _ := m.terminalTurn(r); ended {
				t.Error("raw stop guard=false")
			}
		})
	}
}

func FuzzRereviewPreservation(f *testing.F) {
	f.Add([]byte("public\tβ\r\n"), uint8(0))
	f.Add([]byte("\x00\xff"), uint8(1))
	f.Fuzz(func(t *testing.T, data []byte, mode uint8) {
		if len(data) > 512 {
			return
		}
		prefix := "public:" + fmt.Sprintf("%x", data) + "\tβ\r\n"
		suffix := "\npublic-after:" + fmt.Sprintf("%x", data)
		// Oracle derives the complete expected bytes without consulting implementation spans.
		input := prefix + "-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----" + suffix
		if got := Redact(input); got != prefix+privateHidden+suffix {
			t.Error("exact outside bytes=false")
		}
		if mode%2 == 0 {
			input += suffix + "\n-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----" + suffix
			if Redact(input) != prefix+privateHidden+suffix+suffix+"\n"+privateHidden+suffix {
				t.Error("multiple-block outside bytes=false")
			}
		}
		prior := privateMessage(prefix)
		orphan := privateMessage(prefix + "fixture\n-----END PRIVATE KEY-----\n" + input)
		p := privateEventPrivacy{}
		safe, _ := p.project(prior)
		if string(safe) != string(prior) {
			t.Error("public event byte-exact=false")
		}
		safe, _ = p.project(orphan)
		if string(safe) != privateHidden {
			t.Error("orphan event scope=false")
		}
	})
}
