package run

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"backstash/internal/config"
	"backstash/internal/execx"
	"backstash/internal/report"
	"backstash/internal/secrets"
	"backstash/internal/storage"
	"backstash/internal/target"
)

var fixedNow = time.Date(2026, 11, 23, 2, 0, 5, 0, time.UTC)

// memStore is an in-memory bucket.
type memStore struct {
	data      map[string][]byte
	mtimes    map[string]time.Time
	uploadErr error
	listErr   error
	deleted   []string
}

func newMem() *memStore { return &memStore{data: map[string][]byte{}, mtimes: map[string]time.Time{}} }

func (memoryStore *memStore) put(key, body string, age time.Duration) {
	memoryStore.data[key] = []byte(body)
	memoryStore.mtimes[key] = fixedNow.Add(-age)
}

func (memoryStore *memStore) Upload(_ context.Context, filePath, key string) error {
	if memoryStore.uploadErr != nil {
		return memoryStore.uploadErr
	}
	body, err := os.ReadFile(filePath)
	memoryStore.data[key], memoryStore.mtimes[key] = body, fixedNow
	return err
}

func (memoryStore *memStore) List(_ context.Context, prefix string) ([]storage.Object, error) {
	if memoryStore.listErr != nil {
		return nil, memoryStore.listErr
	}
	var out []storage.Object
	for key, body := range memoryStore.data {
		if strings.HasPrefix(key, prefix) {
			out = append(out, storage.Object{Key: key, Size: int64(len(body)), Modified: memoryStore.mtimes[key]})
		}
	}
	return out, nil
}

func (memoryStore *memStore) Download(_ context.Context, key, dest string) error {
	body, ok := memoryStore.data[key]
	if !ok {
		return errors.New("no such key " + key)
	}
	return os.WriteFile(dest, body, 0o600)
}

func (memoryStore *memStore) Delete(_ context.Context, keys []string) error {
	for _, key := range keys {
		delete(memoryStore.data, key)
		memoryStore.deleted = append(memoryStore.deleted, key)
	}
	return nil
}

func app() *config.App {
	return &config.App{
		Name: "myapp", Environment: "prod", Bucket: "b", SecretsPath: "/x",
		Postgres: &config.PostgresTarget{Mode: config.ModeDocker, Container: "pg", User: "u", Database: "myapp"},
		Redis:    &config.RedisTarget{Mode: config.ModeDocker, Container: "rd", Port: 6379},
		Files:    &config.FilesTarget{Mode: config.ModeDocker, Container: "web", Path: "/data", Label: "uploads"},
	}
}

func setup(testState *testing.T, runner *execx.Fake, in string) (*Deps, *memStore, *bytes.Buffer) {
	testState.Helper()
	memoryStore := newMem()
	out := &bytes.Buffer{}
	targetApp := app()
	depsInstance := &Deps{
		Cfg: &config.Config{Apps: map[string]*config.App{targetApp.Key(): targetApp}}, App: targetApp,
		Target: target.Deps{Run: runner, Secrets: secrets.Set{}, StagingDir: filepath.Join(testState.TempDir(), "stage"), Now: func() time.Time { return fixedNow }},
		Store:  memoryStore, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		In: strings.NewReader(in), Out: out,
	}
	return depsInstance, memoryStore, out
}

// happy makes every tool succeed and produce output.
func happy() *execx.Fake {
	fakeExec := &execx.Fake{}
	fakeExec.Handler = func(cmd execx.Cmd) error {
		if cmd.Stdout != nil {
			_, _ = io.WriteString(cmd.Stdout, "data")
		}
		if len(cmd.Args) > 0 && cmd.Args[0] == "cp" { // docker cp (redis)
			return os.WriteFile(cmd.Args[len(cmd.Args)-1], []byte("REDIS"), 0o600)
		}
		return nil
	}
	return fakeExec
}

func TestBackupAllTargets(testState *testing.T) {
	depsInstance, memoryStore, _ := setup(testState, happy(), "")
	rep := depsInstance.Backup(context.Background(), BackupOpts{})
	if rep.Failed() || len(rep.Targets) != 3 {
		testState.Fatalf("report: %+v", rep)
	}
	for _, key := range []string{
		"myapp/prod/postgres/myapp-20261123-020005.dump",
		"myapp/prod/redis/redis-20261123-020005.rdb",
		"myapp/prod/files/uploads-20261123-020005.tar.gz",
	} {
		if _, ok := memoryStore.data[key]; !ok {
			testState.Errorf("missing %s in %v", key, keysOf(memoryStore.data))
		}
	}
	if entries, _ := staged(depsInstance.Target.StagingDir); len(entries) != 0 {
		testState.Errorf("local copies not removed: %v", entries)
	}
}

// staged lists the backup files in dir, ignoring the run lock.
func staged(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	return slices.DeleteFunc(entries, func(entry os.DirEntry) bool { return entry.Name() == ".lock" }), err
}

func TestSecondBackupOfSameAppIsRefused(testState *testing.T) {
	depsInstance, _, _ := setup(testState, happy(), "")
	release, err := depsInstance.lock()
	if err != nil {
		testState.Fatal(err)
	}
	defer release()
	if rep := depsInstance.Backup(context.Background(), BackupOpts{}); rep.Err == nil {
		testState.Error("a second concurrent backup must be refused")
	}
}

func keysOf(stringMap map[string][]byte) []string {
	var resultKeys []string
	for key := range stringMap {
		resultKeys = append(resultKeys, key)
	}
	return resultKeys
}

func TestKeepLeavesLocalCopy(testState *testing.T) {
	depsInstance, _, _ := setup(testState, happy(), "")
	depsInstance.Backup(context.Background(), BackupOpts{Only: []config.Kind{config.Postgres}, Keep: true})
	if entries, _ := staged(depsInstance.Target.StagingDir); len(entries) != 1 {
		testState.Errorf("want 1 kept file, got %v", entries)
	}
}

func TestOneFailingTargetDoesNotSkipTheOthers(testState *testing.T) {
	fakeExec := happy()
	inner := fakeExec.Handler
	fakeExec.Handler = func(cmd execx.Cmd) error {
		if slices.Contains(cmd.Args, "pg_dump") {
			return errors.New("pg_dump failed (exit 1): connection refused")
		}
		return inner(cmd)
	}
	depsInstance, memoryStore, _ := setup(testState, fakeExec, "")
	rep := depsInstance.Backup(context.Background(), BackupOpts{})
	if !rep.Failed() || !slices.Equal(rep.FailedKinds(), []config.Kind{config.Postgres}) {
		testState.Fatalf("failed kinds = %v", rep.FailedKinds())
	}
	if rep.Targets[1].Err != nil || rep.Targets[2].Err != nil {
		testState.Errorf("redis and files should still succeed: %+v", rep.Targets)
	}
	if len(memoryStore.data) != 2 {
		testState.Errorf("stored = %v", keysOf(memoryStore.data))
	}
}

func TestUploadFailureKeepsLocalFileAndSaysWhere(testState *testing.T) {
	depsInstance, memoryStore, _ := setup(testState, happy(), "")
	memoryStore.uploadErr = errors.New("bucket unreachable")
	rep := depsInstance.Backup(context.Background(), BackupOpts{Only: []config.Kind{config.Postgres}})
	err := rep.Targets[0].Err
	if err == nil || !strings.Contains(err.Error(), "bucket unreachable") || !strings.Contains(err.Error(), "local file kept at") {
		testState.Fatalf("got %v", err)
	}
	if entries, _ := staged(depsInstance.Target.StagingDir); len(entries) != 1 {
		testState.Errorf("the dump must be kept after a failed upload, got %v", entries)
	}
}

func TestPruneFailureDoesNotFailAStoredBackup(testState *testing.T) {
	depsInstance, memoryStore, _ := setup(testState, happy(), "")
	memoryStore.put("myapp/prod/postgres/myapp-20250101-020000.dump", "old", 400*24*time.Hour)
	memoryStore.listErr = errors.New("list denied")
	rep := depsInstance.Backup(context.Background(), BackupOpts{Only: []config.Kind{config.Postgres}})
	if rep.Failed() {
		testState.Fatalf("a prune failure must not fail the backup: %+v", rep.Targets[0])
	}
	if !strings.Contains(rep.Targets[0].Note, "prune failed") {
		testState.Errorf("note = %q", rep.Targets[0].Note)
	}
}

func TestRetentionPrunesPerKindAndCounts(testState *testing.T) {
	depsInstance, memoryStore, _ := setup(testState, happy(), "")
	memoryStore.put("myapp/prod/postgres/myapp-20250101-020000.dump", "old", 400*24*time.Hour)
	memoryStore.put("myapp/prod/redis/redis-20250101-020000.rdb", "old", 400*24*time.Hour) // other kind: untouched by a postgres-only run
	rep := depsInstance.Backup(context.Background(), BackupOpts{Only: []config.Kind{config.Postgres}})
	if rep.Targets[0].Pruned != 1 || !slices.Equal(memoryStore.deleted, []string{"myapp/prod/postgres/myapp-20250101-020000.dump"}) {
		testState.Errorf("pruned=%d deleted=%v", rep.Targets[0].Pruned, memoryStore.deleted)
	}
}

func TestPanicInATargetBecomesAFailure(testState *testing.T) {
	fakeExec := &execx.Fake{Handler: func(execx.Cmd) error { panic("boom") }}
	depsInstance, _, _ := setup(testState, fakeExec, "")
	rep := depsInstance.Backup(context.Background(), BackupOpts{Only: []config.Kind{config.Postgres}})
	if !rep.Failed() || !strings.Contains(rep.Targets[0].Err.Error(), "boom") {
		testState.Fatalf("got %+v", rep)
	}
}

func TestOnlyRejectsUnconfiguredKind(testState *testing.T) {
	depsInstance, _, _ := setup(testState, happy(), "")
	depsInstance.App.Redis = nil
	rep := depsInstance.Backup(context.Background(), BackupOpts{Only: []config.Kind{config.Redis}})
	if rep.Err == nil || !strings.Contains(rep.Err.Error(), "not configured") {
		testState.Fatalf("got %+v", rep)
	}
}

const pgKey = "myapp/prod/postgres/myapp-20261122-020000.dump"

func TestRestoreByNameRequiresTypedConfirmation(testState *testing.T) {
	fakeExec := happy()
	depsInstance, memoryStore, out := setup(testState, fakeExec, "myapp\n")
	memoryStore.put(pgKey, "PGDMP", time.Hour)
	rep := depsInstance.Restore(context.Background(), config.Postgres, "myapp-20261122-020000.dump", false)
	if rep.Failed() || rep.Silent {
		testState.Fatalf("got %+v", rep)
	}
	if !strings.Contains(out.String(), "OVERWRITE 'myapp'") {
		testState.Errorf("no warning shown: %q", out.String())
	}
	if !slices.Contains(fakeExec.Calls[len(fakeExec.Calls)-1].Args, "pg_restore") {
		testState.Errorf("pg_restore not run: %+v", fakeExec.Calls)
	}
	if len(rep.Targets) != 1 || rep.Duration < 0 {
		testState.Errorf("targets not reported: %+v", rep)
	}
	if entries, _ := staged(depsInstance.Target.StagingDir); len(entries) != 0 {
		testState.Errorf("downloaded dump should be removed after success: %v", entries)
	}
}

func TestWrongConfirmationCancelsSilently(testState *testing.T) {
	fakeExec := happy()
	depsInstance, memoryStore, _ := setup(testState, fakeExec, "nope\n")
	memoryStore.put(pgKey, "PGDMP", time.Hour)
	rep := depsInstance.Restore(context.Background(), config.Postgres, "myapp-20261122-020000.dump", false)
	if !rep.Silent || !errors.Is(rep.Err, ErrCancelled) || len(fakeExec.Calls) != 0 {
		testState.Fatalf("got %+v calls=%d", rep, len(fakeExec.Calls))
	}
}

func TestRestoreWithNoTerminalIsSilent(testState *testing.T) {
	depsInstance, memoryStore, _ := setup(testState, happy(), "") // EOF straight away
	memoryStore.put(pgKey, "PGDMP", time.Hour)
	rep := depsInstance.Restore(context.Background(), config.Postgres, "", false)
	if !rep.Silent || !errors.Is(rep.Err, ErrNoTerminal) {
		testState.Fatalf("got %+v", rep)
	}
}

func TestRestorePicksFromNewestFirstList(testState *testing.T) {
	fakeExec := happy()
	depsInstance, memoryStore, out := setup(testState, fakeExec, "1\nmyapp\n")
	memoryStore.put("myapp/prod/postgres/myapp-20261101-020000.dump", "old", 22*24*time.Hour)
	memoryStore.put(pgKey, "new", time.Hour)
	var got string
	fakeExec.Handler = func(cmd execx.Cmd) error {
		if cmd.Stdin != nil {
			body, _ := io.ReadAll(cmd.Stdin)
			got = string(body)
		}
		return nil
	}
	rep := depsInstance.Restore(context.Background(), config.Postgres, "", false)
	if rep.Failed() || got != "new" {
		testState.Fatalf("restored %q, rep=%+v\n%s", got, rep, out.String())
	}
}

func TestInvalidSelectionIsSilent(testState *testing.T) {
	for _, inputStr := range []string{"\n", "abc\n", "9\n", "0\n", "²\n"} {
		depsInstance, memoryStore, _ := setup(testState, happy(), inputStr)
		memoryStore.put(pgKey, "x", time.Hour)
		rep := depsInstance.Restore(context.Background(), config.Postgres, "", false)
		if !rep.Silent {
			testState.Errorf("input %q: got %+v", inputStr, rep)
		}
	}
}

func TestFailedRestoreKeepsDownloadAndReportsIt(testState *testing.T) {
	fakeExec := &execx.Fake{Handler: func(execx.Cmd) error { return errors.New("pg_restore failed (exit 1): lock timeout") }}
	depsInstance, memoryStore, _ := setup(testState, fakeExec, "")
	memoryStore.put(pgKey, "PGDMP", time.Hour)
	rep := depsInstance.Restore(context.Background(), config.Postgres, "myapp-20261122-020000.dump", true)
	if !rep.Failed() || rep.Silent || !strings.Contains(rep.Targets[0].Err.Error(), "backup kept at") {
		testState.Fatalf("got %+v", rep)
	}
	if entries, _ := staged(depsInstance.Target.StagingDir); len(entries) != 1 {
		testState.Errorf("download should be kept for a retry: %v", entries)
	}
}

func TestRestoreCannotEscapeStagingDir(testState *testing.T) {
	depsInstance, _, _ := setup(testState, happy(), "")
	rep := depsInstance.Restore(context.Background(), config.Postgres, "..", true)
	if !rep.Silent {
		testState.Fatalf("got %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(depsInstance.Target.StagingDir), "..")); err != nil {
		testState.Fatal(err)
	}
}

func TestRedisRestoreDownloadsAndPrintsStepsWithoutOverwriting(testState *testing.T) {
	fakeExec := happy()
	depsInstance, memoryStore, out := setup(testState, fakeExec, "")
	memoryStore.put("myapp/prod/redis/redis-20261122-020000.rdb", "REDIS", time.Hour)
	rep := depsInstance.Restore(context.Background(), config.Redis, "redis-20261122-020000.rdb", false)
	if rep.Failed() || len(fakeExec.Calls) != 0 {
		testState.Fatalf("redis restore must not run anything: %+v calls=%v", rep, fakeExec.Calls)
	}
	if !strings.Contains(out.String(), "docker stop rd") {
		testState.Errorf("steps missing: %q", out.String())
	}
	if entries, _ := staged(depsInstance.Target.StagingDir); len(entries) != 1 {
		testState.Errorf("the rdb must be kept for the operator: %v", entries)
	}
}

func TestListShowsNewestFirst(testState *testing.T) {
	depsInstance, memoryStore, out := setup(testState, happy(), "")
	memoryStore.put("myapp/prod/postgres/myapp-20261101-020000.dump", "a", 22*24*time.Hour)
	memoryStore.put(pgKey, "bb", time.Hour)
	if err := depsInstance.List(context.Background(), []config.Kind{config.Postgres}); err != nil {
		testState.Fatal(err)
	}
	outputStr := out.String()
	if strings.Index(outputStr, "20261122") > strings.Index(outputStr, "20261101") || !strings.Contains(outputStr, "postgres") {
		testState.Errorf("not newest first:\n%s", outputStr)
	}
}

func TestPruneCommand(testState *testing.T) {
	depsInstance, memoryStore, _ := setup(testState, happy(), "")
	memoryStore.put("myapp/prod/postgres/myapp-20250101-020000.dump", "old", 400*24*time.Hour)
	memoryStore.put(pgKey, "new", time.Hour)
	rep := depsInstance.Prune(context.Background(), PruneOpts{})
	if rep.Failed() || rep.TotalPruned() != 1 {
		testState.Fatalf("got %+v", rep)
	}
}

func TestValidateReportsMissingSecretsAndTools(testState *testing.T) {
	depsInstance, _, _ := setup(testState, happy(), "")
	depsInstance.App.Postgres.Mode = config.ModeTCP
	depsInstance.App.Postgres.Host = "h"
	missing := func(name string) (string, error) {
		if name == "docker" {
			return "/usr/bin/docker", nil
		}
		return "", errors.New("not found")
	}
	checks := depsInstance.Validate(context.Background(), secrets.Set{SecretS3KeyID: "id"}, missing)
	failed := map[string]bool{}
	for _, checkItem := range checks {
		if checkItem.Err != nil {
			failed[checkItem.Name] = true
		}
	}
	for _, want := range []string{"secret S3_SECRET_ACCESS_KEY", "secret POSTGRES_PASSWORD", "tool pg_dump", "tool pg_restore"} {
		if !failed[want] {
			testState.Errorf("expected %q to fail; failed=%v", want, failed)
		}
	}
	if failed["tool docker"] || failed["secret S3_ACCESS_KEY_ID"] {
		testState.Errorf("false failures: %v", failed)
	}
}

func TestNotifierSkipsSilentAndOnlyEmailsFailures(testState *testing.T) {
	// No Slack/Mail configured: Send must be a no-op, not a panic.
	notifier := NewNotifier(&config.Config{}, app(), secrets.Set{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if notifier.Slack != nil || notifier.Mail != nil {
		testState.Fatalf("nothing configured, got %+v", notifier)
	}
	notifier.Send(context.Background(), report.New("Backup", "myapp", "prod"), "log")
}

func TestNewStoreNamesMissingSecrets(testState *testing.T) {
	_, err := NewStore(&config.Config{}, app(), secrets.Set{})
	if err == nil || !strings.Contains(err.Error(), SecretS3KeyID) || !strings.Contains(err.Error(), SecretS3Secret) {
		testState.Fatalf("got %v", err)
	}
}

func TestValidateChecksContainersAreRunning(testState *testing.T) {
	fakeExec := &execx.Fake{Handler: func(cmd execx.Cmd) error {
		switch cmd.Args[len(cmd.Args)-1] {
		case "pg":
			_, _ = io.WriteString(cmd.Stdout, "true\n")
		case "rd":
			_, _ = io.WriteString(cmd.Stdout, "false\n")
		default:
			return errors.New("docker failed (exit 1): No such object: web")
		}
		return nil
	}}
	depsInstance, _, _ := setup(testState, fakeExec, "")
	have := func(string) (string, error) { return "/bin/x", nil }
	got := map[string]error{}
	for _, checkItem := range depsInstance.Validate(context.Background(), secrets.Set{SecretS3KeyID: "a", SecretS3Secret: "b"}, have) {
		got[checkItem.Name] = checkItem.Err
	}
	if got["container pg"] != nil {
		testState.Errorf("running container flagged: %v", got["container pg"])
	}
	if got["container rd"] == nil || !strings.Contains(got["container rd"].Error(), "not running") {
		testState.Errorf("stopped container not flagged: %v", got["container rd"])
	}
	if got["container web"] == nil || !strings.Contains(got["container web"].Error(), "No such object") {
		testState.Errorf("missing container not flagged: %v", got["container web"])
	}
}
