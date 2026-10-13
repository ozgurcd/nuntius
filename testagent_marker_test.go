package nuntius

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const testAgentStartEnvironment = "NUNTIUS_TEST_AGENT_START_DIR"
const testAgentStartPointer = ".test-agent-start-dir"

// Only the isolated bootstrap process changes its environment. The shared
// parallel test process never sets a fixture-specific destination.
func testAgentStartBootstrap(args []string) error {
	if len(args) != 2 || (args[0] != "__nuntius_fork" && args[0] != "__nuntius_exec" && args[0] != "__nuntius_launchd_exec") {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(args[1], "launch.json"))
	var spec launchSpec
	if err != nil || json.Unmarshal(b, &spec) != nil {
		return fmt.Errorf("offline marker bootstrap unavailable")
	}
	b, err = os.ReadFile(filepath.Join(spec.Workdir, testAgentStartPointer))
	if os.IsNotExist(err) {
		return os.Unsetenv(testAgentStartEnvironment)
	}
	if err != nil || !filepath.IsAbs(string(b)) {
		return fmt.Errorf("offline marker destination unavailable")
	}
	return os.Setenv(testAgentStartEnvironment, string(b))
}

func testAgentStartDir(workdir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(workdir, testAgentStartPointer))
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(string(b)) || string(b) == workdir {
		return "", fmt.Errorf("offline marker destination invalid")
	}
	return string(b), nil
}

func testAgentStartOptIn(t *testing.T, m *Manager) string {
	t.Helper()
	dir := t.TempDir()
	for _, lane := range m.config.Lanes {
		if err := os.WriteFile(filepath.Join(lane.Workdir, testAgentStartPointer), []byte(dir), 0600); err != nil {
			t.Fatal("offline marker pointer unavailable")
		}
	}
	// Registered after TempDir: child teardown precedes destination removal.
	linkedCleanup(t, m)
	return dir
}

func testAgentStartEnv(dir *string) []string {
	env := []string{}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, testAgentStartEnvironment+"=") {
			env = append(env, value)
		}
	}
	if dir != nil {
		env = append(env, testAgentStartEnvironment+"="+*dir)
	}
	return env
}

func testAgentStartRefusal(t *testing.T, dir *string, want int) {
	t.Helper()
	cmd := exec.Command(fakeBinary, "exec", "resume", "fixture-thread", "--sandbox", "workspace-write", "--json", "-")
	cmd.Env = testAgentStartEnv(dir)
	cmd.Stdin = strings.NewReader("hello")
	// Cmd.Dir stays unset to exercise the inherited working directory.
	err := cmd.Run()
	if err == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != want {
		t.Fatalf("offline strict refusal did not exit %d", want)
	}
}

func testAgentStartNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal("offline marker directory unavailable")
	}
	names := []string{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "agent-start-") {
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() || info.Size() != 0 || info.Mode().Perm() != 0600 {
				t.Fatal("offline marker must be an empty private regular file")
			}
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestFakeAgentStartMarkerIsolation(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	before := testAgentStartNames(t, cwd)
	dir := t.TempDir()
	for want := 1; want <= 2; want++ {
		testAgentStartRefusal(t, &dir, 2)
		if !reflect.DeepEqual(before, testAgentStartNames(t, cwd)) {
			t.Fatal("fake added an inherited-CWD marker")
		}
		if got := len(testAgentStartNames(t, dir)); got != want {
			t.Fatalf("configured private markers=%d want=%d", got, want)
		}
	}
}

func TestFakeAgentStartMarkerUnset(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	before := testAgentStartNames(t, cwd)
	testAgentStartRefusal(t, nil, 2)
	if !reflect.DeepEqual(before, testAgentStartNames(t, cwd)) {
		t.Fatal("unset oracle added an inherited-CWD marker")
	}
}

func TestFakeAgentStartMarkerInvalid(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	before := testAgentStartNames(t, cwd)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"", "relative", filepath.Join(t.TempDir(), "missing"), file} {
		testAgentStartRefusal(t, &dir, 47)
		if !reflect.DeepEqual(before, testAgentStartNames(t, cwd)) {
			t.Fatal("invalid oracle fell back to inherited CWD")
		}
	}
}

func TestFakeAgentStartParallelFixtures(t *testing.T) {
	t.Parallel()
	var ready sync.WaitGroup
	ready.Add(2)
	for want := 1; want <= 2; want++ {
		t.Run(fmt.Sprint(want), func(t *testing.T) {
			goalEndParallel(t)
			// Always release the barrier, including a fixture setup failure.
			arrived := false
			defer func() {
				if !arrived {
					ready.Done()
				}
			}()
			m, _, _, _ := linkedHeld(t)
			dir, err := testAgentStartDir(m.config.Lanes[0].Workdir)
			if err != nil {
				t.Fatal(err)
			}
			if want == 2 {
				testAgentStartRefusal(t, &dir, 2)
			}
			ready.Done()
			arrived = true
			ready.Wait()
			linkedStarts(t, m, want)
			if got := len(testAgentStartNames(t, dir)); got != want {
				t.Fatalf("parallel fixture markers=%d want=%d", got, want)
			}
		})
	}
}

func TestFakeAgentStartMissingOracle(t *testing.T) {
	if _, err := testAgentStartDir(t.TempDir()); err == nil {
		t.Fatal("missing oracle destination was accepted")
	}
}
