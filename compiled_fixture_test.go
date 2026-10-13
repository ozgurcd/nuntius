package nuntius

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestCompiledFixtureReuse(t *testing.T) {
	goalEndParallel(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestCompiledFixtureReuseHelper$", "--", "compiled-fixture-helper")
	cmd.Env = append(os.Environ(), "NUNTIUS_TEST_FAKE_BINARY="+fakeBinary, "NUNTIUS_TEST_CLI_BINARY="+cliBinary)
	if err := cmd.Run(); err != nil {
		t.Fatal("nested helper did not reuse compiled fixtures")
	}
}

func TestCompiledFixtureReuseHelper(t *testing.T) {
	if os.Args[len(os.Args)-1] != "compiled-fixture-helper" {
		return
	}
	for _, pair := range [][2]string{
		{fakeBinary, os.Getenv("NUNTIUS_TEST_FAKE_BINARY")},
		{cliBinary, os.Getenv("NUNTIUS_TEST_CLI_BINARY")},
	} {
		got, err := os.Stat(pair[0])
		want, wantErr := os.Stat(pair[1])
		if err != nil || wantErr != nil || !got.Mode().IsRegular() || got.Mode()&0111 == 0 || !os.SameFile(got, want) {
			t.Fatal("helper replaced or lost an original compiled fixture")
		}
	}
}
