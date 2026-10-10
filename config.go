package nuntius

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const Version = "v0.2.4"

type Lane struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Workdir      string   `json:"workdir"`
	Binary       string   `json:"binary"`
	ExtraArgs    []string `json:"extra_args"`
	StartMode    string   `json:"start_mode,omitempty"`
	ContinueText string   `json:"continue_text,omitempty"`
}

type Config struct {
	Lanes         []Lane               `json:"lanes,omitempty"`
	Pools         map[string]AgentPool `json:"pools,omitempty"`
	OversightPath string               `json:"oversight_path,omitempty"`
	SourcePath    string               `json:"-"`
	MaxRuntime    string               `json:"max_runtime"`
	ReportsDir    string               `json:"reports_dir"`
	Notifications *bool                `json:"notifications,omitempty"`
	AutoResume    *bool                `json:"auto_resume,omitempty"`
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,63}$`)
var goalPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9.-]{1,63}$`)
var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func strictJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return errors.New("invalid JSON or unknown field")
	}
	var more any
	if d.Decode(&more) != io.EOF {
		return errors.New("expected one JSON document")
	}
	return nil
}
func LoadConfig(path string) (Config, error) {
	var c Config
	f, e := os.Open(path)
	if e != nil {
		return c, errors.New("cannot open configuration")
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 65537))
	if e != nil || len(b) > 65536 {
		return c, errors.New("configuration exceeds 64 KiB or cannot be read")
	}
	if e = strictJSON(b, &c); e != nil {
		return c, e
	}
	if c.Pools != nil && len(c.Lanes) > 0 {
		return c, errors.New("use pools or lanes, not both")
	}
	_, e = c.Validate()
	if e == nil {
		c.Lanes, e = c.expanded()
		c.SourcePath = path
	}
	return c, e
}
func (c Config) Validate() (time.Duration, error) {
	if c.OversightPath != "" && !filepath.IsAbs(c.OversightPath) {
		return 0, errors.New("oversight_path must be absolute")
	}
	if c.Pools != nil {
		lanes, e := c.expanded()
		if e != nil {
			return 0, e
		}
		// Validate even disabled templates, so enabling a pool cannot bypass policy.
		for kind, p := range c.Pools {
			l := p.Template
			l.Kind = kind
			l.Name = kind + "-template"
			lanes = append(lanes, l)
		}
		c.Lanes = lanes
	}
	fail := func() (time.Duration, error) {
		return 0, errors.New("invalid lane configuration; check names, absolute paths, duration and permitted extra_args")
	}
	d, e := time.ParseDuration(c.MaxRuntime)
	if e != nil || d <= 0 || d > 7*24*time.Hour {
		return fail()
	}
	if (len(c.Lanes) < 1 && c.Pools == nil) || len(c.Lanes) > 32 || !filepath.IsAbs(c.ReportsDir) {
		return fail()
	}
	seen := map[string]bool{}
	for _, l := range c.Lanes {
		if (l.StartMode != "" && l.StartMode != "as_is" && l.StartMode != "goal_then_continue") || (l.Kind == "codex" && l.StartMode == "goal_then_continue") || len(l.ContinueText) > 12000 {
			return fail()
		}
		if !namePattern.MatchString(l.Name) || seen[l.Name] || (l.Kind != "claude" && l.Kind != "codex") || !filepath.IsAbs(l.Binary) || !filepath.IsAbs(l.Workdir) {
			return fail()
		}
		seen[l.Name] = true
		b, e := os.Stat(l.Binary)
		if e != nil || !b.Mode().IsRegular() || b.Mode().Perm()&0111 == 0 {
			return fail()
		}
		w, e := os.Stat(l.Workdir)
		if e != nil || !w.IsDir() {
			return fail()
		}
		// Preserve normal hooks/settings. Refuse options that can indirectly override
		// permissions, output format, working directory or session selection.
		if len(l.ExtraArgs) > 16 {
			return fail()
		}
		for i := 0; i < len(l.ExtraArgs); i++ {
			key, val, eq := strings.Cut(l.ExtraArgs[i], "=")
			if l.Kind == "codex" && key == "--skip-git-repo-check" && !eq {
				continue
			}
			if key != "--model" && key != "-m" && !(l.Kind == "claude" && (key == "--effort" || key == "--max-budget-usd" || key == "--permission-mode")) && !(l.Kind == "codex" && key == "--sandbox") {
				return fail()
			}
			if !eq {
				i++
				if i >= len(l.ExtraArgs) {
					return fail()
				}
				val = l.ExtraArgs[i]
			}
			if key == "--permission-mode" && val != "auto" && val != "acceptEdits" && val != "plan" && val != "manual" {
				return fail()
			}
			if key == "--sandbox" && val != "read-only" && val != "workspace-write" {
				return fail()
			}
			if val == "" || len(val) > 128 || strings.HasPrefix(val, "-") || strings.ContainsAny(val, "\x00\r\n") || strings.Contains(strings.ToLower(val), "bypass") || strings.Contains(val, "danger-full-access") {
				return fail()
			}
		}
	}
	return d, nil
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`),
	regexp.MustCompile(`(?:ghp_|gho_|ghs_|github_pat_|sk-ant-|sk-|xox[a-z]-)[A-Za-z0-9_-]*`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]*\.eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]*`),
	regexp.MustCompile(`(?im)^.*(?:password|secret|token)\s*=.*$`),
}

func (l Lane) mode() string {
	if l.StartMode != "" {
		return l.StartMode
	}
	if l.Kind == "claude" {
		return "goal_then_continue"
	}
	return "as_is"
}
func (l Lane) continuation() string {
	if l.ContinueText != "" {
		return l.ContinueText
	}
	return "continue"
}

func Redact(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}
func bounded(s string, n int) string {
	s = Redact(s)
	if len(s) > n {
		s = s[:n-len("\n[truncated]")] + "\n[truncated]"
	}
	return strings.ToValidUTF8(s, "")
}
