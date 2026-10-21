//go:build integration

// Package integration runs the real tools (pg_dump, pg_restore, redis-cli, docker, tar) against real
// Postgres, Redis and MinIO. CI starts them (.github/workflows/ci.yml); to run it locally start the
// same containers and export the IT_* variables listed in need() calls below. Without them every test
// skips, so `go test -tags integration ./...` is safe anywhere.
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"backstash/internal/config"
	"backstash/internal/execx"
	"backstash/internal/run"
	"backstash/internal/secrets"
	"backstash/internal/storage"
	"backstash/internal/target"
)

func need(testState *testing.T, name string) string {
	testState.Helper()
	envVal := os.Getenv(name)
	if envVal == "" {
		testState.Skipf("%s not set: integration services are not running", name)
	}
	return envVal
}

// sh runs a command and fails the test with its output if it does not succeed.
func sh(testState *testing.T, name string, args ...string) string {
	testState.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		testState.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func psql(testState *testing.T, container, sql string) string {
	testState.Helper()
	return sh(testState, "docker", "exec", container, "psql", "-U", "postgres", "-d", "appdb", "-tAc", sql)
}

func newDeps(testState *testing.T, app *config.App, sec secrets.Set) (*run.Deps, *storage.S3) {
	testState.Helper()
	store := storage.NewS3(storage.S3Config{
		Endpoint: need(testState, "IT_S3_ENDPOINT"), Region: "us-east-1", Bucket: need(testState, "IT_S3_BUCKET"),
		AccessKeyID: need(testState, "IT_S3_KEY"), SecretAccessKey: need(testState, "IT_S3_SECRET"),
	})
	days := 30
	app.RetentionDays = &days
	return &run.Deps{
		Cfg:    &config.Config{Apps: map[string]*config.App{app.Key(): app}},
		App:    app,
		Target: target.Deps{Run: execx.Exec{}, Secrets: sec, StagingDir: filepath.Join(testState.TempDir(), "stage")},
		Store:  store,
		Log:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
		In:     strings.NewReader(""),
		Out:    io.Discard,
	}, store
}

// uniqueApp keeps tests from seeing each other's objects in the shared bucket.
func uniqueApp(testState *testing.T) *config.App {
	return &config.App{
		Name:        fmt.Sprintf("it%d", time.Now().UnixNano()),
		Environment: "test",
	}
}

func listOne(testState *testing.T, store *storage.S3, prefix string) storage.Object {
	testState.Helper()
	objs, err := store.List(context.Background(), prefix)
	if err != nil {
		testState.Fatal(err)
	}
	if len(objs) != 1 {
		testState.Fatalf("want exactly 1 object under %s, got %v", prefix, objs)
	}
	return objs[0]
}

func seedPostgres(testState *testing.T, container string) {
	testState.Helper()
	psql(testState, container, "DROP TABLE IF EXISTS t; CREATE TABLE t(id int); INSERT INTO t SELECT generate_series(1,3)")
}

// TestDockerModes backs up and restores a Postgres, a Redis and a folder that all live in containers.
func TestDockerModes(testState *testing.T) {
	pgContainer, rdContainer, filesContainer := need(testState, "IT_PG_CONTAINER"), need(testState, "IT_REDIS_CONTAINER"), need(testState, "IT_FILES_CONTAINER")
	seedPostgres(testState, pgContainer)
	sh(testState, "docker", "exec", rdContainer, "redis-cli", "set", "greeting", "hello")
	sh(testState, "docker", "exec", filesContainer, "sh", "-c", "mkdir -p /data && echo kept > /data/a.txt")

	app := uniqueApp(testState)
	app.Postgres = &config.PostgresTarget{Mode: config.ModeDocker, Container: pgContainer, User: "postgres", Database: "appdb"}
	app.Redis = &config.RedisTarget{Mode: config.ModeDocker, Container: rdContainer, Port: 6379}
	app.Files = &config.FilesTarget{Mode: config.ModeDocker, Container: filesContainer, Path: "/data", Label: "uploads"}
	depsInstance, store := newDeps(testState, app, secrets.Set{})
	ctx := context.Background()

	rep := depsInstance.Backup(ctx, run.BackupOpts{})
	if rep.Failed() {
		testState.Fatalf("backup failed: %+v", rep)
	}
	pgObj := listOne(testState, store, app.Prefix(config.Postgres))
	rdObj := listOne(testState, store, app.Prefix(config.Redis))
	fileObj := listOne(testState, store, app.Prefix(config.Files))

	// The Redis snapshot is a real RDB, and the container is left clean.
	rdb := filepath.Join(testState.TempDir(), "x.rdb")
	if err := store.Download(ctx, rdObj.Key, rdb); err != nil {
		testState.Fatal(err)
	}
	if body, _ := os.ReadFile(rdb); !strings.HasPrefix(string(body), "REDIS") {
		testState.Errorf("not an RDB file: %q", body[:min(len(body), 8)])
	}
	if out, err := exec.Command("docker", "exec", rdContainer, "ls", "/tmp/backstash-dump.rdb").CombinedOutput(); err == nil {
		testState.Errorf("snapshot left inside the container: %s", out)
	}

	// Break things, then restore them.
	psql(testState, pgContainer, "DELETE FROM t")
	sh(testState, "docker", "exec", filesContainer, "rm", "/data/a.txt")

	if restoreRep := depsInstance.Restore(ctx, config.Postgres, path.Base(pgObj.Key), true); restoreRep.Failed() {
		testState.Fatalf("postgres restore: %+v", restoreRep)
	}
	if got := psql(testState, pgContainer, "SELECT count(*) FROM t"); got != "3" {
		testState.Errorf("rows after restore = %s, want 3", got)
	}
	if restoreRep := depsInstance.Restore(ctx, config.Files, path.Base(fileObj.Key), true); restoreRep.Failed() {
		testState.Fatalf("files restore: %+v", restoreRep)
	}
	if got := sh(testState, "docker", "exec", filesContainer, "cat", "/data/a.txt"); got != "kept" {
		testState.Errorf("restored file = %q", got)
	}

	if checks := depsInstance.Validate(ctx, secrets.Set{run.SecretS3KeyID: "x", run.SecretS3Secret: "y"}, nil); hasFailure(checks, "container") {
		testState.Errorf("validate flagged a running container: %+v", checks)
	}
}

// TestTCPModes uses the host's pg_dump and redis-cli over the network and a plain host folder.
func TestTCPModes(testState *testing.T) {
	pgContainer := need(testState, "IT_PG_CONTAINER")
	host, pgPort, rdPort := need(testState, "IT_HOST"), need(testState, "IT_PG_PORT"), need(testState, "IT_REDIS_PORT")
	seedPostgres(testState, pgContainer)

	dir := filepath.Join(testState.TempDir(), "uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		testState.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("kept"), 0o644); err != nil {
		testState.Fatal(err)
	}

	postgresPort, _ := strconv.Atoi(pgPort)
	redisPort, _ := strconv.Atoi(rdPort)
	app := uniqueApp(testState)
	app.Postgres = &config.PostgresTarget{Mode: config.ModeTCP, Host: host, Port: postgresPort, User: "postgres", Database: "appdb"}
	app.Redis = &config.RedisTarget{Mode: config.ModeTCP, Host: host, Port: redisPort}
	app.Files = &config.FilesTarget{Mode: config.ModePath, Path: dir, Label: "uploads"}
	depsInstance, store := newDeps(testState, app, secrets.Set{"POSTGRES_PASSWORD": need(testState, "IT_PG_PASSWORD")})
	ctx := context.Background()

	if rep := depsInstance.Backup(ctx, run.BackupOpts{}); rep.Failed() {
		testState.Fatalf("backup failed: %+v", rep)
	}
	pgObj := listOne(testState, store, app.Prefix(config.Postgres))
	listOne(testState, store, app.Prefix(config.Redis))
	fileObj := listOne(testState, store, app.Prefix(config.Files))

	psql(testState, pgContainer, "DELETE FROM t")
	_ = os.Remove(filepath.Join(dir, "a.txt"))

	if restoreRep := depsInstance.Restore(ctx, config.Postgres, path.Base(pgObj.Key), true); restoreRep.Failed() {
		testState.Fatalf("postgres restore: %+v", restoreRep)
	}
	if got := psql(testState, pgContainer, "SELECT count(*) FROM t"); got != "3" {
		testState.Errorf("rows after restore = %s, want 3", got)
	}
	if restoreRep := depsInstance.Restore(ctx, config.Files, path.Base(fileObj.Key), true); restoreRep.Failed() {
		testState.Fatalf("files restore: %+v", restoreRep)
	}
	if body, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(body) != "kept" {
		testState.Errorf("restored file = %q, %v", body, err)
	}
}

// TestWrongPasswordIsAFailureNotAPanic checks the failure path against a real server.
func TestWrongPasswordIsAFailureNotAPanic(testState *testing.T) {
	need(testState, "IT_PG_CONTAINER")
	host, pgPort := need(testState, "IT_HOST"), need(testState, "IT_PG_PORT")
	postgresPort, _ := strconv.Atoi(pgPort)
	app := uniqueApp(testState)
	app.Postgres = &config.PostgresTarget{Mode: config.ModeTCP, Host: host, Port: postgresPort, User: "postgres", Database: "appdb"}
	depsInstance, store := newDeps(testState, app, secrets.Set{"POSTGRES_PASSWORD": "definitely-wrong"})

	rep := depsInstance.Backup(context.Background(), run.BackupOpts{})
	if !rep.Failed() || rep.Targets[0].Err == nil {
		testState.Fatalf("a wrong password must fail the target: %+v", rep)
	}
	if objs, _ := store.List(context.Background(), app.Prefix(config.Postgres)); len(objs) != 0 {
		testState.Errorf("a failed dump must upload nothing, got %v", objs)
	}
	// .lock is the per-app run lock (kept on purpose); anything else would be a leftover dump.
	for _, entry := range mustReadDir(testState, depsInstance.Target.StagingDir) {
		if entry.Name() != ".lock" {
			testState.Errorf("a failed dump must leave no local file, found %s", entry.Name())
		}
	}
}

func hasFailure(checks []run.Check, prefix string) bool {
	for _, checkItem := range checks {
		if strings.HasPrefix(checkItem.Name, prefix) && checkItem.Err != nil {
			return true
		}
	}
	return false
}

func mustReadDir(testState *testing.T, dir string) []os.DirEntry {
	testState.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		testState.Fatal(err)
	}
	return entries
}
