package nuntius

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
)

// effectiveIdentity describes the next launch without consulting agent settings.
func effectiveIdentity(l Lane) (model, effort string) {
	model, effort = "default", "default"
	if l.Kind == "opencode" {
		return identityValue(l.Model), effort
	}
	for i := 0; i < len(l.ExtraArgs); i++ {
		flag, value, equal := strings.Cut(l.ExtraArgs[i], "=")
		if flag != "--model" && flag != "-m" && !(flag == "--effort" && l.Kind == "claude") {
			continue
		}
		if !equal && i+1 < len(l.ExtraArgs) {
			i++
			value = l.ExtraArgs[i]
		}
		if flag == "--effort" {
			effort = identityValue(value)
		} else {
			model = identityValue(value)
		}
	}
	return model, effort
}

func identityValue(value string) string {
	if value == "" {
		return "default"
	}
	return cell(bounded(Redact(value), 128))
}

// Only startup metadata can report effort. Assistant/tool text is never parsed
// as configuration. exec versions that omit this field remain at default.
func codexEffort(b []byte) string {
	for _, line := range bytes.Split(b, []byte("\n")) {
		var v struct {
			Type        string `json:"type"`
			Effort      string `json:"reasoning_effort"`
			ModelEffort string `json:"model_reasoning_effort"`
			Payload     struct {
				Type        string `json:"type"`
				Effort      string `json:"reasoning_effort"`
				ModelEffort string `json:"model_reasoning_effort"`
			} `json:"payload"`
		}
		if len(line) > 65536 || json.Unmarshal(line, &v) != nil {
			continue
		}
		kind := v.Type
		if kind == "event_msg" {
			kind = v.Payload.Type
		}
		switch kind {
		case "session_meta", "session_config", "session_configured", "session.started", "thread.started":
		default:
			continue
		}
		for _, value := range []string{v.Effort, v.ModelEffort, v.Payload.Effort, v.Payload.ModelEffort} {
			switch value {
			case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
				return value
			}
		}
	}
	return ""
}

func (m *Manager) captureEffort(r *Run) bool {
	if r.Kind != "codex" || (r.Effort != "" && r.Effort != "default") {
		return false
	}
	b, err := readWindow(filepath.Join(m.store, "runs", r.ID, "events.jsonl"), false)
	if err != nil {
		return false
	}
	if effort := m.eventWindow(r.ID, b, true).effort; effort != "" {
		r.Effort = effort
		return true
	}
	return false
}
