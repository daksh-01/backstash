package target

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"backstash/internal/config"
	"backstash/internal/execx"
	"backstash/internal/secrets"
)

var fixedNow = time.Date(2026, 11, 23, 2, 0, 5, 0, time.UTC)

func deps(testState *testing.T, fakeExec *execx.Fake) Deps {
	testState.Helper()
	return Deps{
		Run:        fakeExec,
		Secrets:    secrets.Set{"POSTGRES_PASSWORD": "pgpass", "REDIS_PASSWORD": "rdpass"},
		StagingDir: filepath.Join(testState.TempDir(), "stage"),
		Now:        func() time.Time { return fixedNow },
	}
}

// writes makes a Fake that writes body to the command's stdout.
func writes(body string) func(execx.Cmd) error {
	return func(cmd execx.Cmd) error {
		if cmd.Stdout != nil {
			_, _ = io.WriteString(cmd.Stdout, body)
		}
		return nil
	}
}

func noSecretInArgv(testState *testing.T, cmd execx.Cmd, secrets ...string) {
	testState.Helper()
	for _, secretVal := range secrets {
		if slices.ContainsFunc(cmd.Args, func(argItem string) bool { return strings.Contains(argItem, secretVal) }) {
			testState.Errorf("secret %q leaked into argv: %v", secretVal, cmd.Args)
		}
	}
}

func TestBackupName(testState *testing.T) {
	if got := BackupName("myapp", ".dump", fixedNow); got != "myapp-20261123-020005.dump" {
		testState.Errorf("got %q", got)
	}
}

func TestPostgresDumpTCP(testState *testing.T) {
	fakeExec := &execx.Fake{Handler: writes("PGDMP")}
	pg := &config.PostgresTarget{Mode: config.ModeTCP, Host: "db.internal", Port: 5432, User: "app", Database: "appdb"}
	artifact, err := deps(testState, fakeExec).DumpPostgres(context.Background(), pg)
	if err != nil {
		testState.Fatal(err)
	}
	callCmd := fakeExec.Calls[0]
	want := []string{"-h", "db.internal", "-p", "5432", "-U", "app", "-Fc", "appdb"}
	if callCmd.Name != "pg_dump" || !slices.Equal(callCmd.Args, want) {
		testState.Errorf("cmd = %s %v", callCmd.Name, callCmd.Args)
	}
	if !slices.Contains(callCmd.Env, "PGPASSWORD=pgpass") {
		testState.Errorf("password not passed via env: %v", callCmd.Env)
	}
	noSecretInArgv(testState, callCmd, "pgpass")
	if artifact.Name != "appdb-20261123-020005.dump" || artifact.Size != 5 {
		testState.Errorf("artifact = %+v", artifact)
	}
	if info, _ := os.Stat(artifact.Path); info.Mode().Perm() != 0o600 {
		testState.Errorf("dump mode = %v", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Dir(artifact.Path)); info.Mode().Perm() != 0o700 {
		testState.Errorf("staging dir mode = %v", info.Mode().Perm())
	}
}

func TestPostgresDumpDocker(testState *testing.T) {
	fakeExec := &execx.Fake{Handler: writes("PGDMP")}
	pg := &config.PostgresTarget{Mode: config.ModeDocker, Container: "pg1", User: "app", Database: "appdb"}
	if _, err := deps(testState, fakeExec).DumpPostgres(context.Background(), pg); err != nil {
		testState.Fatal(err)
	}
	callCmd := fakeExec.Calls[0]
	want := []string{"exec", "-e", "PGPASSWORD", "pg1", "pg_dump", "-U", "app", "-Fc", "appdb"} // no stdin, so no -i
	if callCmd.Name != "docker" || !slices.Equal(callCmd.Args, want) {
		testState.Errorf("cmd = %s %v", callCmd.Name, callCmd.Args)
	}
	noSecretInArgv(testState, callCmd, "pgpass")
}

func TestPostgresTCPNeedsPassword(testState *testing.T) {
	targetDeps := deps(testState, &execx.Fake{})
	targetDeps.Secrets = secrets.Set{}
	pg := &config.PostgresTarget{Mode: config.ModeTCP, Host: "h", Port: 5432, User: "u", Database: "d"}
	_, err := targetDeps.DumpPostgres(context.Background(), pg)
	var missing ErrNoSecret
	if !errors.As(err, &missing) || missing.Name != "POSTGRES_PASSWORD" {
		testState.Fatalf("got %v", err)
	}
}

func TestFailedDumpLeavesNoFile(testState *testing.T) {
	fakeExec := &execx.Fake{Handler: func(cmd execx.Cmd) error {
		_, _ = io.WriteString(cmd.Stdout, "partial")
		return errors.New("pg_dump failed (exit 1): connection refused")
	}}
	targetDeps := deps(testState, fakeExec)
	pg := &config.PostgresTarget{Mode: config.ModeDocker, Container: "c", User: "u", Database: "d"}
	if _, err := targetDeps.DumpPostgres(context.Background(), pg); err == nil || !strings.Contains(err.Error(), "connection refused") {
		testState.Fatalf("got %v", err)
	}
	if entries, _ := os.ReadDir(targetDeps.StagingDir); len(entries) != 0 {
		testState.Errorf("partial dump left behind: %v", entries)
	}
}

func TestEmptyDumpIsAFailure(testState *testing.T) {
	targetDeps := deps(testState, &execx.Fake{})
	pg := &config.PostgresTarget{Mode: config.ModeDocker, Container: "c", User: "u", Database: "d"}
	if _, err := targetDeps.DumpPostgres(context.Background(), pg); err == nil || !strings.Contains(err.Error(), "empty") {
		testState.Fatalf("got %v", err)
	}
	if entries, _ := os.ReadDir(targetDeps.StagingDir); len(entries) != 0 {
		testState.Errorf("empty dump left behind")
	}
}

func TestPostgresRestoreIsSingleTransactionWithLockTimeout(testState *testing.T) {
	fakeExec := &execx.Fake{}
	targetDeps := deps(testState, fakeExec)
	dump := filepath.Join(testState.TempDir(), "x.dump")
	_ = os.WriteFile(dump, []byte("PGDMP"), 0o600)
	pg := &config.PostgresTarget{Mode: config.ModeDocker, Container: "pg1", User: "app", Database: "appdb"}
	if err := targetDeps.RestorePostgres(context.Background(), pg, dump); err != nil {
		testState.Fatal(err)
	}
	callCmd := fakeExec.Calls[0]
	for _, flag := range []string{"--single-transaction", "--clean", "--if-exists", "--no-owner", "PGOPTIONS", "pg_restore"} {
		if !slices.Contains(callCmd.Args, flag) {
			testState.Errorf("missing %s in %v", flag, callCmd.Args)
		}
	}
	if !slices.Contains(callCmd.Env, "PGOPTIONS=-c lock_timeout=30s") {
		testState.Errorf("lock timeout missing: %v", callCmd.Env)
	}
	if callCmd.Stdin == nil {
		testState.Error("dump not piped to stdin")
	}
	if !slices.Equal(callCmd.Args[:6], []string{"exec", "-i", "-e", "PGPASSWORD", "-e", "PGOPTIONS"}) {
		testState.Errorf("restore must keep stdin open and forward both variables by name: %v", callCmd.Args[:6])
	}
}

func TestPostgresRestoreFailureSaysDBUnchanged(testState *testing.T) {
	fakeExec := &execx.Fake{Handler: func(execx.Cmd) error { return errors.New("pg_restore failed (exit 1): lock timeout") }}
	dump := filepath.Join(testState.TempDir(), "x.dump")
	_ = os.WriteFile(dump, []byte("x"), 0o600)
	pg := &config.PostgresTarget{Mode: config.ModeDocker, Container: "c", User: "u", Database: "d"}
	err := deps(testState, fakeExec).RestorePostgres(context.Background(), pg, dump)
	if err == nil || !strings.Contains(err.Error(), "should be unchanged") {
		testState.Fatalf("got %v", err)
	}
}

func TestFilesDockerAndPath(testState *testing.T) {
	for _, testCase := range []struct {
		name     string
		tgt      *config.FilesTarget
		wantName string
		wantArgs []string
	}{
		{"docker", &config.FilesTarget{Mode: config.ModeDocker, Container: "app", Path: "/data", Label: "uploads"},
			"docker", []string{"exec", "app", "tar", "-czf", "-", "-C", "/data", "."}},
		{"path", &config.FilesTarget{Mode: config.ModePath, Path: "/srv/up", Label: "uploads"},
			"tar", []string{"-czf", "-", "-C", "/srv/up", "."}},
	} {
		testState.Run(testCase.name, func(subTestState *testing.T) {
			fakeExec := &execx.Fake{Handler: writes("gzdata")}
			artifact, err := deps(subTestState, fakeExec).DumpFiles(context.Background(), testCase.tgt)
			if err != nil {
				subTestState.Fatal(err)
			}
			// Call 0 asks the target's tar for its version; the dump is the last call.
			dump := fakeExec.Calls[len(fakeExec.Calls)-1]
			if dump.Name != testCase.wantName || !slices.Equal(dump.Args, testCase.wantArgs) {
				subTestState.Errorf("cmd = %s %v", dump.Name, dump.Args)
			}
			if artifact.Name != "uploads-20261123-020005.tar.gz" {
				subTestState.Errorf("name = %s", artifact.Name)
			}
		})
	}
}

// --warning is GNU-only: BusyBox tar (Alpine images) rejects it, so it must be used only when supported.
func TestFilesDumpUsesGNUWarningFlagOnlyForGNUTar(testState *testing.T) {
	tgt := &config.FilesTarget{Mode: config.ModeDocker, Container: "app", Path: "/data", Label: "uploads"}
	for _, testCase := range []struct {
		name     string
		version  func(execx.Cmd) error
		wantFlag bool
	}{
		{"gnu", func(c execx.Cmd) error { _, _ = io.WriteString(c.Stdout, "tar (GNU tar) 1.35\n"); return nil }, true},
		{"busybox rejects --version", func(execx.Cmd) error { return errors.New("tar: unrecognized option: version") }, false},
		{"busybox prints usage", func(c execx.Cmd) error {
			_, _ = io.WriteString(c.Stdout, "BusyBox v1.37.0 multi-call binary\n")
			return nil
		}, false},
	} {
		testState.Run(testCase.name, func(subTestState *testing.T) {
			fakeExec := &execx.Fake{Handler: func(c execx.Cmd) error {
				if slices.Contains(c.Args, "--version") {
					return testCase.version(c)
				}
				return writes("gzdata")(c)
			}}
			if _, err := deps(subTestState, fakeExec).DumpFiles(context.Background(), tgt); err != nil {
				subTestState.Fatal(err)
			}
			dump := fakeExec.Calls[len(fakeExec.Calls)-1]
			if got := slices.Contains(dump.Args, "--warning=no-file-changed"); got != testCase.wantFlag {
				subTestState.Errorf("flag present = %v, want %v: %v", got, testCase.wantFlag, dump.Args)
			}
		})
	}
}

func TestFilesRestoreDockerUsesInteractiveStdin(testState *testing.T) {
	fakeExec := &execx.Fake{}
	arch := filepath.Join(testState.TempDir(), "a.tar.gz")
	_ = os.WriteFile(arch, []byte("x"), 0o600)
	tgt := &config.FilesTarget{Mode: config.ModeDocker, Container: "app", Path: "/data", Label: "filesTarget"}
	if err := deps(testState, fakeExec).RestoreFiles(context.Background(), tgt, arch); err != nil {
		testState.Fatal(err)
	}
	want := []string{"exec", "-i", "app", "tar", "-xzf", "-", "-C", "/data"}
	if !slices.Equal(fakeExec.Calls[0].Args, want) {
		testState.Errorf("args = %v", fakeExec.Calls[0].Args)
	}
}

func TestRedisDockerCopiesOutAndCleansUp(testState *testing.T) {
	fakeExec := &execx.Fake{}
	fakeExec.Handler = func(cmd execx.Cmd) error {
		if len(cmd.Args) > 0 && cmd.Args[0] == "cp" { // docker cp writes the destination file
			return os.WriteFile(cmd.Args[len(cmd.Args)-1], []byte("REDIS0011"), 0o644)
		}
		return nil
	}
	redisTarget := &config.RedisTarget{Mode: config.ModeDocker, Container: "rd", Port: 6379}
	artifact, err := deps(testState, fakeExec).DumpRedis(context.Background(), redisTarget)
	if err != nil {
		testState.Fatal(err)
	}
	if len(fakeExec.Calls) != 3 {
		testState.Fatalf("calls = %d: %+v", len(fakeExec.Calls), fakeExec.Calls)
	}
	dumpCall, cpCall, rmCall := fakeExec.Calls[0], fakeExec.Calls[1], fakeExec.Calls[2]
	if !slices.Equal(dumpCall.Args, []string{"exec", "-e", "REDISCLI_AUTH", "rd", "redis-cli", "-p", "6379", "--rdb", containerRDB}) {
		testState.Errorf("dump args = %v", dumpCall.Args)
	}
	if !slices.Contains(dumpCall.Env, "REDISCLI_AUTH=rdpass") {
		testState.Errorf("auth not in env: %v", dumpCall.Env)
	}
	noSecretInArgv(testState, dumpCall, "rdpass")
	if cpCall.Args[0] != "cp" || cpCall.Args[1] != "rd:"+containerRDB {
		testState.Errorf("cp args = %v", cpCall.Args)
	}
	if rmCall.Args[len(rmCall.Args)-1] != containerRDB || !slices.Contains(rmCall.Args, "rm") {
		testState.Errorf("cleanup args = %v", rmCall.Args)
	}
	if info, _ := os.Stat(artifact.Path); info.Mode().Perm() != 0o600 {
		testState.Errorf("rdb mode = %v, want 0600", info.Mode().Perm())
	}
	if artifact.Name != "redis-20261123-020005.rdb" {
		testState.Errorf("name = %s", artifact.Name)
	}
}

func TestRedisDockerCleansUpEvenWhenDumpFails(testState *testing.T) {
	fakeExec := &execx.Fake{Handler: func(cmd execx.Cmd) error {
		if slices.Contains(cmd.Args, "--rdb") {
			return errors.New("redis-cli failed (exit 1): NOAUTH")
		}
		return nil
	}}
	targetDeps := deps(testState, fakeExec)
	_, err := targetDeps.DumpRedis(context.Background(), &config.RedisTarget{Mode: config.ModeDocker, Container: "rd"})
	if err == nil || !strings.Contains(err.Error(), "NOAUTH") {
		testState.Fatalf("got %v", err)
	}
	if !slices.Contains(fakeExec.Calls[len(fakeExec.Calls)-1].Args, "rm") {
		testState.Error("container snapshot not cleaned up after failure")
	}
	if entries, _ := os.ReadDir(targetDeps.StagingDir); len(entries) != 0 {
		testState.Errorf("reserved file left behind: %v", entries)
	}
}

func TestRedisTCP(testState *testing.T) {
	fakeExec := &execx.Fake{}
	fakeExec.Handler = func(cmd execx.Cmd) error { return os.WriteFile(cmd.Args[len(cmd.Args)-1], []byte("REDIS"), 0o644) }
	redisTarget := &config.RedisTarget{Mode: config.ModeTCP, Host: "cache.internal", Port: 6380}
	if _, err := deps(testState, fakeExec).DumpRedis(context.Background(), redisTarget); err != nil {
		testState.Fatal(err)
	}
	callCmd := fakeExec.Calls[0]
	if callCmd.Name != "redis-cli" || !slices.Equal(callCmd.Args[:4], []string{"-h", "cache.internal", "-p", "6380"}) || callCmd.Args[4] != "--rdb" {
		testState.Errorf("cmd = %s %v", callCmd.Name, callCmd.Args)
	}
	noSecretInArgv(testState, callCmd, "rdpass")
}

func TestRedisRestoreStepsMentionContainer(testState *testing.T) {
	steps := RedisRestoreSteps(&config.RedisTarget{Mode: config.ModeDocker, Container: "rd"}, "/tmp/x.rdb")
	for _, want := range []string{"docker stop rd", "docker cp /tmp/x.rdb rd:/data/dump.rdb", "docker start rd"} {
		if !strings.Contains(steps, want) {
			testState.Errorf("missing %q in:\n%s", want, steps)
		}
	}
}

func TestValidateRDB(testState *testing.T) {
	validPath := filepath.Join(testState.TempDir(), "valid.rdb")
	_ = os.WriteFile(validPath, []byte("REDIS0011data"), 0o600)
	if err := ValidateRDB(validPath); err != nil {
		testState.Fatalf("expected valid RDB to pass, got %v", err)
	}

	corruptPath := filepath.Join(testState.TempDir(), "corrupt.rdb")
	_ = os.WriteFile(corruptPath, []byte("NOTREDIS"), 0o600)
	if err := ValidateRDB(corruptPath); err == nil || !strings.Contains(err.Error(), "missing REDIS magic header") {
		testState.Fatalf("expected error for corrupted RDB, got %v", err)
	}

	missingPath := filepath.Join(testState.TempDir(), "missing.rdb")
	if err := ValidateRDB(missingPath); err == nil {
		testState.Fatal("expected error for missing file")
	}
}
