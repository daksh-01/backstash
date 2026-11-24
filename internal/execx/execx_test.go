package execx

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
)

func TestExecRunsAndPassesEnvStdinStdout(testState *testing.T) {
	var out bytes.Buffer
	err := Exec{}.Run(context.Background(), Cmd{
		Name: "sh", Args: []string{"-c", `cat; printf '%s' "$SECRET"`},
		Env: []string{"SECRET=s3cr3t"}, Stdin: strings.NewReader("in:"), Stdout: &out,
	})
	if err != nil || out.String() != "in:s3cr3t" {
		testState.Fatalf("out=%q err=%v", out.String(), err)
	}
}

func TestExecErrorCarriesStderrAndExitCode(testState *testing.T) {
	err := Exec{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo bad thing >&2; exit 3"}})
	if err == nil || !strings.Contains(err.Error(), "exit 3") || !strings.Contains(err.Error(), "bad thing") {
		testState.Fatalf("got %v", err)
	}
}

func TestExecMissingBinary(testState *testing.T) {
	err := Exec{}.Run(context.Background(), Cmd{Name: "definitely-not-installed-xyz"})
	if err == nil || !strings.Contains(err.Error(), "is it installed") {
		testState.Fatalf("got %v", err)
	}
}

func TestInContainerForwardsEnvByNameNeverValue(testState *testing.T) {
	in := Cmd{Name: "pg_dump", Args: []string{"-Fc", "db"}, Env: []string{"PGPASSWORD=hunter2", "PGOPTIONS=-c x=1"}}
	got := InContainer("pg1", in)
	want := []string{"exec", "-e", "PGPASSWORD", "-e", "PGOPTIONS", "pg1", "pg_dump", "-Fc", "db"}
	if got.Name != "docker" || !slices.Equal(got.Args, want) {
		testState.Errorf("got %s %v", got.Name, got.Args)
	}
	if strings.Contains(strings.Join(got.Args, " "), "hunter2") {
		testState.Error("secret value leaked into argv")
	}
	if !slices.Equal(got.Env, in.Env) {
		testState.Errorf("values must stay in the process env so docker can forward them: %v", got.Env)
	}
}

func TestInContainerAddsInteractiveOnlyWithStdin(testState *testing.T) {
	if containerArgs := InContainer("container1", Cmd{Name: "tar"}).Args; slices.Contains(containerArgs, "-i") {
		testState.Errorf("-i without stdin: %v", containerArgs)
	}
	if containerArgs := InContainer("container1", Cmd{Name: "tar", Stdin: strings.NewReader("x")}).Args; containerArgs[1] != "-i" {
		testState.Errorf("-i missing with stdin: %v", containerArgs)
	}
}
