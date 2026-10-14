package nuntius

import (
	"bytes"
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

func testAgentStartCheckPackageDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("offline package marker check unavailable")
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "agent-start-") {
			return fmt.Errorf("offline suite left an agent-start marker in the package directory")
		}
	}
	return nil
}

// TestMain calls this after all tests and owned children finish, including
// parallel tests and their cleanups. Any marker makes the entire suite fail.
func testAgentStartSuiteResult(code int) int {
	dir, err := os.Getwd()
	if err == nil {
		err = testAgentStartCheckPackageDir(dir)
	}
	if err != nil {
		fmt.Println("offline suite package marker check failed")
		return 1
	}
	return code
}

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

func TestFakeAgentStartMarkerWorkingDirectory(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(fmt.Sprint(alias), func(t *testing.T) {
			workdir := t.TempDir()
			dir := workdir
			if alias {
				dir = filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(workdir, dir); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(fakeBinary, "exec", "resume", "fixture-thread", "--sandbox", "workspace-write", "--json", "-")
			cmd.Dir = workdir
			cmd.Env = testAgentStartEnv(&dir)
			cmd.Stdin = strings.NewReader("hello")
			if err := cmd.Run(); err == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 47 {
				t.Fatal("working-directory marker destination was not refused")
			}
			if len(testAgentStartNames(t, workdir)) != 0 {
				t.Fatal("fake wrote a working-directory marker")
			}
		})
	}
}

func TestFakeAgentStartPackageDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := testAgentStartCheckPackageDir(dir); err != nil {
		t.Fatal("clean package directory was refused")
	}
	if err := os.WriteFile(filepath.Join(dir, "ordinary-file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := testAgentStartCheckPackageDir(dir); err != nil {
		t.Fatal("ordinary package file was refused")
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-start-fixture"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := testAgentStartCheckPackageDir(dir); err == nil {
		t.Fatal("package marker leak was accepted")
	}
	if err := testAgentStartCheckPackageDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("unavailable package directory passed the marker check")
	}
}

func TestFakeAgentStartSuiteEnd(t *testing.T) {
	const probe = "NUNTIUS_TEST_AGENT_SUITE_LEAK"
	if mode, nested := os.LookupEnv(probe); nested {
		if mode == "1" {
			// The parent owns this subprocess's cwd through t.TempDir. Leave
			// the marker until TestMain checks the completed inner suite.
			if err := os.WriteFile("agent-start-fixture", nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	for _, want := range []int{0, 1} {
		t.Run(fmt.Sprint(want), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestFakeAgentStartSuiteEnd$", "-test.count=1")
			cmd.Dir = t.TempDir()
			cmd.Env = append(testAgentStartEnv(nil), probe+"="+fmt.Sprint(want))
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			err := cmd.Run()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != want || (err != nil && want == 0) {
				t.Fatalf("completed inner suite did not exit %d", want)
			}
			if bytes.Contains(output.Bytes(), []byte("offline suite package marker check failed")) != (want == 1) {
				t.Fatal("suite-end leak verdict did not match the fixture")
			}
		})
	}
}
