package run

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"backstash/internal/config"
	"backstash/internal/execx"
	"backstash/internal/report"
	"backstash/internal/secrets"
)

func pgOnly() []config.Kind { return []config.Kind{config.Postgres} }

func TestEncryptedBackupRoundTrip(testState *testing.T) {
	id, _ := age.GenerateX25519Identity()
	var restored string
	fakeExec := happy()
	inner := fakeExec.Handler
	fakeExec.Handler = func(cmd execx.Cmd) error {
		if cmd.Stdin != nil {
			raw, _ := io.ReadAll(cmd.Stdin)
			restored = string(raw)
		}
		return inner(cmd)
	}
	deps, store, _ := setup(testState, fakeExec, "")
	deps.Cfg.Host.AgeRecipient = id.Recipient().String()

	rep := deps.Backup(context.Background(), BackupOpts{Only: pgOnly()})
	if rep.Failed() {
		testState.Fatalf("%+v", rep.Targets)
	}
	key := "myapp/prod/postgres/myapp-20261123-020005.dump.age"
	if body, ok := store.data[key]; !ok || string(body) == "data" {
		testState.Fatalf("expected ciphertext at %s, have %v", key, keysOf(store.data))
	}
	if entries, _ := staged(deps.Target.StagingDir); len(entries) != 0 {
		testState.Fatalf("no plaintext may be left behind: %v", entries)
	}

	// restore needs the private key
	if rep := deps.Restore(context.Background(), config.Postgres, "myapp-20261123-020005.dump.age", true); !rep.Failed() ||
		!strings.Contains(rep.Targets[0].Err.Error(), SecretAgeIdentity) {
		testState.Fatalf("restore without key: %+v", rep)
	}
	deps.Target.Secrets = secrets.Set{SecretAgeIdentity: id.String()}
	if rep := deps.Restore(context.Background(), config.Postgres, "myapp-20261123-020005.dump.age", true); rep.Failed() {
		testState.Fatalf("%+v %v", rep, rep.Targets)
	}
	if restored != "data" {
		testState.Fatalf("pg_restore got %q", restored)
	}
}

func TestVerifyChecksNewestBackup(testState *testing.T) {
	deps, store, _ := setup(testState, happy(), "")
	if rep := deps.Verify(context.Background()); !rep.Failed() || !strings.Contains(rep.Targets[0].Err.Error(), "no backups") {
		testState.Fatalf("empty bucket must fail: %+v", rep.Targets)
	}
	store.put(pgKey, "PGDMP", time.Hour)
	store.put("myapp/prod/redis/redis-20261122-020000.rdb", "REDIS0011", time.Hour)
	store.put("myapp/prod/files/uploads-20261122-020000.tar.gz", "not a tarball", time.Hour)
	rep := deps.Verify(context.Background())
	if rep.Targets[0].Err != nil || rep.Targets[1].Err != nil {
		testState.Fatalf("postgres and redis should pass: %+v", rep.Targets)
	}
	if rep.Targets[2].Err == nil {
		testState.Fatal("a corrupt archive must fail")
	}
	if entries, _ := staged(deps.Target.StagingDir); len(entries) != 0 {
		testState.Fatalf("verify must clean up: %v", entries)
	}
}

func TestVerifyFailsWhenPgRestoreCannotReadTheDump(testState *testing.T) {
	fakeExec := happy()
	fakeExec.Handler = func(execx.Cmd) error { return os.ErrInvalid }
	deps, store, _ := setup(testState, fakeExec, "")
	store.put(pgKey, "garbage", time.Hour)
	deps.App.Redis, deps.App.Files = nil, nil
	if rep := deps.Verify(context.Background()); !rep.Failed() {
		testState.Fatal("unreadable dump must fail verify")
	}
}

func TestSuspiciouslySmallBackupIsFlaggedAndVerifyFailsIt(testState *testing.T) {
	deps, store, _ := setup(testState, happy(), "")
	big := strings.Repeat("x", 1000)
	for day := 1; day <= 3; day++ {
		store.put("myapp/prod/postgres/myapp-2026111"+string(rune('0'+day))+"-020000.dump", big, time.Duration(day)*24*time.Hour)
	}
	rep := deps.Backup(context.Background(), BackupOpts{Only: pgOnly()})
	if rep.Failed() || !strings.Contains(rep.Targets[0].Note, "median") {
		testState.Fatalf("want a warning, not a failure: %+v", rep.Targets[0])
	}
	store.put("myapp/prod/postgres/myapp-20261124-020000.dump", "tiny", 0)
	deps.App.Redis, deps.App.Files = nil, nil
	if rep := deps.Verify(context.Background()); !rep.Failed() {
		testState.Fatal("verify must fail a tiny newest backup")
	}
}

func TestBackupRefusesWhenDiskIsTooFull(testState *testing.T) {
	deps, _, _ := setup(testState, happy(), "")
	deps.FreeBytes = func(string) (uint64, error) { return 1 << 20, nil }
	rep := deps.Backup(context.Background(), BackupOpts{Only: pgOnly()})
	if err := rep.Targets[0].Err; err == nil || !strings.Contains(err.Error(), "not enough free space") {
		testState.Fatalf("got %v", err)
	}
}

func TestPruneDryRunAndSafetyLimit(testState *testing.T) {
	deps, store, _ := setup(testState, happy(), "")
	deps.App.Redis, deps.App.Files = nil, nil
	for day := 1; day <= 12; day++ {
		store.put("myapp/prod/postgres/myapp-202501"+twoDigits(day)+"-020000.dump", "x", 400*24*time.Hour)
	}
	store.put(pgKey, "new", time.Hour)

	rep := deps.Prune(context.Background(), PruneOpts{})
	if rep.Targets[0].Err == nil || !strings.Contains(rep.Targets[0].Err.Error(), "limit") || len(store.deleted) != 0 {
		testState.Fatalf("over the limit must refuse: %+v deleted=%d", rep.Targets[0], len(store.deleted))
	}
	rep = deps.Prune(context.Background(), PruneOpts{DryRun: true, Force: true})
	if rep.Failed() || len(store.deleted) != 0 || !strings.Contains(rep.Targets[0].Note, "would remove 12") {
		testState.Fatalf("dry run: %+v", rep.Targets[0])
	}
	rep = deps.Prune(context.Background(), PruneOpts{Force: true})
	if rep.Failed() || rep.TotalPruned() != 12 {
		testState.Fatalf("force: %+v", rep.Targets[0])
	}
}

func twoDigits(n int) string { return string(rune('0'+n/10)) + string(rune('0'+n%10)) }

func TestMetricsFile(testState *testing.T) {
	dir := testState.TempDir()
	rep := report.New(report.ActionBackup, "myapp", "prod")
	rep.Targets = []report.Target{{Kind: config.Postgres, Size: 2048}, {Kind: config.Redis, Err: io.ErrUnexpectedEOF}}
	if err := WriteMetrics(dir, rep, time.Unix(1700000000, 0)); err != nil {
		testState.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "backstash_myapp_prod.prom"))
	if err != nil {
		testState.Fatal(err)
	}
	for _, want := range []string{
		`backstash_run_timestamp_seconds{app="myapp",env="prod"} 1700000000`,
		`backstash_run_success{app="myapp",env="prod"} 0`,
		`backstash_target_success{app="myapp",env="prod",kind="postgres"} 1`,
		`backstash_target_success{app="myapp",env="prod",kind="redis"} 0`,
		`backstash_target_size_bytes{app="myapp",env="prod",kind="postgres"} 2048`,
	} {
		if !strings.Contains(string(raw), want) {
			testState.Errorf("missing %q in:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), `size_bytes{app="myapp",env="prod",kind="redis"}`) {
		testState.Error("a failed target has no size")
	}
}

func TestHeartbeatPingsSuccessAndFailureForBackupsOnly(testState *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { paths = append(paths, r.URL.Path) }))
	defer srv.Close()
	deps, _, _ := setup(testState, happy(), "")
	notifier := Notifier{Log: deps.Log, Heartbeat: srv.URL + "/hc"}

	ok := report.New(report.ActionBackup, "a", "prod")
	bad := report.New(report.ActionBackup, "a", "prod")
	bad.Err = io.EOF
	notifier.Send(context.Background(), ok, "")
	notifier.Send(context.Background(), bad, "")
	notifier.Send(context.Background(), report.New(report.ActionPrune, "a", "prod"), "")
	if len(paths) != 2 || paths[0] != "/hc" || paths[1] != "/hc/fail" {
		testState.Fatalf("paths %v", paths)
	}
}
