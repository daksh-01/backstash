package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(testState *testing.T, dir, rel, body string) {
	testState.Helper()
	targetPath := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		testState.Fatal(err)
	}
	if err := os.WriteFile(targetPath, []byte(body), 0o644); err != nil {
		testState.Fatal(err)
	}
}

const hostYAML = `
s3: {endpoint: "https://x.r2.cloudflarestorage.com", region: auto}
alert_email_to: alerts@example.com
smtp: {host: smtp.example.com, port: 587}
`

const myappYAML = `
app: myapp
bucket: myapp-backups
secrets_path: /backstash/myapp-prod
postgres: {mode: docker, container: pg, user: myapp, database: myapp}
redis: {mode: docker, container: rd}
files: {mode: docker, container: app, path: /var/lib/myapp/data, label: myapp-uploads}
`

func TestLoadValid(testState *testing.T) {
	dir := testState.TempDir()
	write(testState, dir, "config.yml", hostYAML)
	write(testState, dir, "apps.d/myapp-prod.yml", myappYAML)

	cfg, err := Load(dir)
	if err != nil {
		testState.Fatal(err)
	}
	app, err := cfg.Lookup("myapp", "")
	if err != nil {
		testState.Fatal(err)
	}
	if app.Environment != "prod" || app.Postgres.Port != 5432 || app.Redis.Port != 6379 {
		testState.Errorf("defaults not applied: %+v", app)
	}
	if got := cfg.Retention(app); got != 30 {
		testState.Errorf("retention = %d, want 30", got)
	}
	if got := app.Prefix(Redis); got != "myapp/prod/redis/" {
		testState.Errorf("prefix = %q", got)
	}
	if len(app.Kinds()) != 3 {
		testState.Errorf("kinds = %v", app.Kinds())
	}
}

func TestRetentionPrecedence(testState *testing.T) {
	seven, zero := 7, 0
	cfg := &Config{Host: Host{RetentionDays: &seven}}
	if got := cfg.Retention(&App{}); got != 7 {
		testState.Errorf("host retention = %d", got)
	}
	if got := cfg.Retention(&App{RetentionDays: &zero}); got != 0 {
		testState.Errorf("app 0 must win (disables pruning), got %d", got)
	}
}

func TestLoadReportsEveryProblem(testState *testing.T) {
	dir := testState.TempDir()
	write(testState, dir, "config.yml", hostYAML)
	write(testState, dir, "apps.d/bad.yml", `
app: Bad App
postgres: {mode: tcp, user: u}
files: {mode: docker, path: relative, label: "a b"}
`)
	_, err := Load(dir)
	if err == nil {
		testState.Fatal("expected errors")
	}
	for _, want := range []string{
		`app "Bad App"`, "bucket is required", "secrets_path is required",
		"postgres.user and postgres.database", "postgres.host is required",
		"files.container is required", "absolute path", `files.label "a b"`,
	} {
		if !strings.Contains(err.Error(), want) {
			testState.Errorf("error missing %q:\n%v", want, err)
		}
	}
}

func TestUnknownKeyRejected(testState *testing.T) {
	dir := testState.TempDir()
	write(testState, dir, "config.yml", hostYAML)
	write(testState, dir, "apps.d/a.yml", myappYAML+"retention_dayz: 3\n")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "retention_dayz") {
		testState.Fatalf("typo should be rejected, got %v", err)
	}
}

func TestDuplicateAppRejected(testState *testing.T) {
	dir := testState.TempDir()
	write(testState, dir, "config.yml", hostYAML)
	write(testState, dir, "apps.d/a.yml", myappYAML)
	write(testState, dir, "apps.d/b.yml", myappYAML)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "already defined") {
		testState.Fatalf("duplicate should be rejected, got %v", err)
	}
}

func TestLookupUnknownListsConfigured(testState *testing.T) {
	cfg := &Config{Apps: map[string]*App{"a/prod": {}}}
	if _, err := cfg.Lookup("nope", ""); err == nil || !strings.Contains(err.Error(), "a/prod") {
		testState.Fatalf("got %v", err)
	}
}

func TestParseKindAndKinds(testState *testing.T) {
	parsedKinds, err := ParseKinds("postgres, files")
	if err != nil || len(parsedKinds) != 2 || parsedKinds[0] != Postgres || parsedKinds[1] != Files {
		testState.Fatalf("ParseKinds failed: %v %v", parsedKinds, err)
	}
	if _, err := ParseKinds("invalid"); err == nil {
		testState.Fatal("expected error for invalid kind")
	}
	empty, err := ParseKinds("")
	if err != nil || empty != nil {
		testState.Fatalf("expected nil for empty, got %v %v", empty, err)
	}
	k, err := ParseKind("redis")
	if err != nil || k != Redis || !k.IsValid() {
		testState.Fatalf("ParseKind redis: %v %v", k, err)
	}
}

func TestAppTargetLabel(testState *testing.T) {
	targetApp := &App{
		Postgres: &PostgresTarget{Database: "pgdb"},
		Redis:    &RedisTarget{},
		Files:    &FilesTarget{Label: "upl"},
	}
	if label := targetApp.TargetLabel(Postgres); label != "pgdb" {
		testState.Errorf("postgres label = %q, want pgdb", label)
	}
	if label := targetApp.TargetLabel(Redis); label != "redis" {
		testState.Errorf("redis label = %q, want redis", label)
	}
	if label := targetApp.TargetLabel(Files); label != "upl" {
		testState.Errorf("files label = %q, want upl", label)
	}
}

func TestTCPPortValidation(testState *testing.T) {
	dir := testState.TempDir()
	write(testState, dir, "config.yml", hostYAML)
	write(testState, dir, "apps.d/badport.yml", `
app: badport
bucket: bkt
secrets_path: /sec
postgres: {mode: tcp, host: db.internal, port: 99999, user: u, database: d}
`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "port must be between") {
		testState.Fatalf("expected port validation error, got %v", err)
	}
}

func TestSecretsPathLeadingSlashValidation(testState *testing.T) {
	dir := testState.TempDir()
	write(testState, dir, "config.yml", hostYAML)
	write(testState, dir, "apps.d/badpath.yml", `
app: badpath
bucket: bkt
secrets_path: relative/path
postgres: {mode: docker, container: pg, user: u, database: d}
`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "secrets_path must start with '/'") {
		testState.Fatalf("expected secrets_path leading slash error, got %v", err)
	}
}
