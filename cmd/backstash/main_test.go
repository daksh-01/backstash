package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"backstash/internal/config"
	"backstash/internal/report"
	"backstash/internal/run"
	"backstash/internal/runlog"
)

func TestParseInterleavedAcceptsFlagsAnywhere(testState *testing.T) {
	for _, args := range [][]string{
		{"myapp", "postgres", "x.dump", "--env", "staging", "--yes"},
		{"--env", "staging", "myapp", "--yes", "postgres", "x.dump"},
		{"myapp", "--env=staging", "postgres", "x.dump", "--yes"},
	} {
		fs := flag.NewFlagSet("testFlagSet", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		env := fs.String("env", "prod", "")
		yes := fs.Bool("yes", false, "")
		pos, err := parseInterleaved(fs, args)
		if err != nil || !slices.Equal(pos, []string{"myapp", "postgres", "x.dump"}) || *env != "staging" || !*yes {
			testState.Errorf("%v -> pos=%v env=%s yes=%v err=%v", args, pos, *env, *yes, err)
		}
	}
}

func TestParseKinds(testState *testing.T) {
	if parsedKinds, err := config.ParseKinds("postgres, files"); err != nil || len(parsedKinds) != 2 {
		testState.Errorf("got %v %v", parsedKinds, err)
	}
	if _, err := config.ParseKinds("mysql"); err == nil {
		testState.Error("unknown kind accepted")
	}
	if parsedKinds, _ := config.ParseKinds(""); parsedKinds != nil {
		testState.Errorf("empty should mean all, got %v", parsedKinds)
	}
}

func invoke(args ...string) (int, string, string) {
	var out, errw bytes.Buffer
	code := realMain(args, strings.NewReader(""), &out, &errw)
	return code, out.String(), errw.String()
}

func TestUsageAndExitCodes(testState *testing.T) {
	if code, _, errOutput := invoke(); code != 2 || !strings.Contains(errOutput, "Usage:") {
		testState.Errorf("no args: %d %q", code, errOutput)
	}
	if code, out, _ := invoke("version"); code != 0 || !strings.Contains(out, "backstash") {
		testState.Errorf("version: %d %q", code, out)
	}
	if code, _, errOutput := invoke("bogus"); code != 2 || !strings.Contains(errOutput, "unknown command") {
		testState.Errorf("bogus: %d %q", code, errOutput)
	}
	if code, _, errOutput := invoke("backup"); code != 2 || !strings.Contains(errOutput, "usage: backstash backup") {
		testState.Errorf("backup w/o app: %d %q", code, errOutput)
	}
	if code, _, errOutput := invoke("restore", "myapp", "mysql"); code != 2 || !strings.Contains(errOutput, "restore target") {
		testState.Errorf("restore bad kind: %d %q", code, errOutput)
	}
	if code, out, _ := invoke("help"); code != 0 || !strings.Contains(out, "Usage:") {
		testState.Errorf("help: %d %q", code, out)
	}
	if code, _, errOutput := invoke("prune"); code != 2 || !strings.Contains(errOutput, "usage: backstash prune") {
		testState.Errorf("prune w/o app: %d %q", code, errOutput)
	}
	if code, _, errOutput := invoke("restore"); code != 2 || !strings.Contains(errOutput, "usage: backstash restore") {
		testState.Errorf("restore w/o args: %d %q", code, errOutput)
	}
}

func TestValidateSingleApp(testState *testing.T) {
	dir := configDir(testState, "http://127.0.0.1:9")
	identity(testState, fakeInfisical(testState))
	code, out, _ := invoke("--config-dir", dir, "validate", "myapp")
	if code != 1 || !strings.Contains(out, "myapp/prod") {
		testState.Errorf("validate single app exit = %d, out = %s", code, out)
	}
	code, out, _ = invoke("--config-dir", dir, "validate", "missingapp")
	if code != 1 || !strings.Contains(out, `no app "missingapp"`) {
		testState.Errorf("validate missing app exit = %d, out = %s", code, out)
	}
}

func TestStartupFailureAlertsWithFallback(testState *testing.T) {
	dir := configDir(testState, "http://127.0.0.1:9")
	var slackCalls int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slackCalls++
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer slackSrv.Close()

	// Write SLACK_BOT_TOKEN to identity.env along with discard Infisical url
	identityFile := filepath.Join(dir, "identity.env")
	_ = os.WriteFile(identityFile, []byte("SLACK_BOT_TOKEN=xoxb-fallback\n"), 0o600)
	identity(testState, "http://127.0.0.1:9")

	cliApp := &cli{
		dir:     dir,
		env:     "prod",
		in:      strings.NewReader(""),
		out:     io.Discard,
		errw:    io.Discard,
		capture: runlog.New(io.Discard),
	}
	cfg, err := config.Load(dir)
	if err != nil {
		testState.Fatal(err)
	}
	targetApp, err := cfg.Lookup("myapp", "prod")
	if err != nil {
		testState.Fatal(err)
	}

	cliApp.notifyStartupFailure(context.Background(), cfg, targetApp, report.ActionBackup, errors.New("infisical is down"))
}

func TestMissingConfigFailsClearly(testState *testing.T) {
	code, _, errOutput := invoke("--config-dir", testState.TempDir(), "backup", "myapp")
	if code != 1 || !strings.Contains(errOutput, "config.yml") {
		testState.Errorf("got %d %q", code, errOutput)
	}
}

// configDir writes a minimal valid config (one Postgres-in-Docker app) and returns its path.
func configDir(testState *testing.T, s3Endpoint string) string {
	testState.Helper()
	dir := testState.TempDir()
	write := func(rel, body string) {
		targetPath := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			testState.Fatal(err)
		}
		if err := os.WriteFile(targetPath, []byte(body), 0o600); err != nil {
			testState.Fatal(err)
		}
	}
	write("config.yml", "s3: {endpoint: \""+s3Endpoint+"\", region: us-east-1}\n")
	write("apps.d/myapp-prod.yml", `
app: myapp
bucket: myapp-backups
secrets_path: /backstash/myapp-prod
postgres: {mode: docker, container: pg, user: u, database: d}
`)
	return dir
}

// identity points the tool at an Infisical URL with a throwaway machine identity.
func identity(testState *testing.T, infisicalURL string) {
	testState.Helper()
	testState.Setenv("INFISICAL_URL", infisicalURL)
	testState.Setenv("INFISICAL_UNIVERSAL_AUTH_CLIENT_ID", "id")
	testState.Setenv("INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET", "sec")
	testState.Setenv("INFISICAL_PROJECT_ID", "proj")
}

func fakeInfisical(testState *testing.T) string {
	testState.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/universal-auth/login", func(respWriter http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(respWriter, `{"accessToken":"tok"}`)
	})
	mux.HandleFunc("/api/v3/secrets/raw", func(respWriter http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(respWriter, `{"secrets":[{"secretKey":"S3_ACCESS_KEY_ID","secretValue":"k"},{"secretKey":"S3_SECRET_ACCESS_KEY","secretValue":"s"}]}`)
	})
	srv := httptest.NewServer(mux)
	testState.Cleanup(srv.Close)
	return srv.URL
}

func TestBackupWithInfisicalDownFailsWithAClearError(testState *testing.T) {
	dir := configDir(testState, "http://127.0.0.1:9")
	identity(testState, "http://127.0.0.1:9") // nothing listens on the discard port
	code, _, errOutput := invoke("--config-dir", dir, "backup", "myapp")
	if code != 1 || !strings.Contains(errOutput, "FAILED:") || !strings.Contains(errOutput, "infisical login") {
		testState.Errorf("got %d %q", code, errOutput)
	}
}

func TestUnknownAppListsWhatIsConfigured(testState *testing.T) {
	dir := configDir(testState, "http://127.0.0.1:9")
	identity(testState, "http://127.0.0.1:9")
	code, _, errOutput := invoke("--config-dir", dir, "list", "nope")
	if code != 1 || !strings.Contains(errOutput, `no app "nope"`) || !strings.Contains(errOutput, "myapp/prod") {
		testState.Errorf("got %d %q", code, errOutput)
	}
}

func TestValidateShowsEveryCheckAndFailsOnTheBucket(testState *testing.T) {
	dir := configDir(testState, "http://127.0.0.1:9") // secrets resolve, the bucket does not exist
	identity(testState, fakeInfisical(testState))
	code, out, _ := invoke("--config-dir", dir, "validate")
	if code != 1 {
		testState.Errorf("exit = %d, want 1\n%s", code, out)
	}
	for _, want := range []string{"ok   config (1 app(s)", "myapp/prod", "ok   secret S3_ACCESS_KEY_ID", "ok   secret S3_SECRET_ACCESS_KEY", "FAIL bucket myapp-backups"} {
		if !strings.Contains(out, want) {
			testState.Errorf("validate output missing %q:\n%s", want, out)
		}
	}
}

func TestFinishPrintsEachTargetAndSetsTheExitCode(testState *testing.T) {
	newCLI := func() (*cli, *bytes.Buffer) {
		var out bytes.Buffer
		return &cli{out: &out, errw: io.Discard, capture: runlog.New(io.Discard)}, &out
	}
	ok := report.Report{Targets: []report.Target{
		{Kind: config.Postgres, Name: "d-20261123-020005.dump", Size: 5_000_000, Pruned: 2},
		{Kind: config.Redis, Name: "redis-20261123-020030.rdb", Size: 1_000_000, Note: "prune failed: x"},
	}}
	cliApp, out := newCLI()
	if code := cliApp.finish(context.Background(), run.Notifier{}, ok); code != 0 {
		testState.Errorf("success exit = %d", code)
	}
	for _, want := range []string{"postgres: d-20261123-020005.dump (5.0 MB)", "removed 2 old backup(s)", "warning: prune failed: x"} {
		if !strings.Contains(out.String(), want) {
			testState.Errorf("missing %q in:\n%s", want, out)
		}
	}

	failed := report.Report{Targets: []report.Target{{Kind: config.Redis, Err: errors.New("NOAUTH")}}}
	cliApp, out = newCLI()
	if code := cliApp.finish(context.Background(), run.Notifier{}, failed); code != 1 || !strings.Contains(out.String(), "redis: FAILED: NOAUTH") {
		testState.Errorf("failure: exit=%d out=%q", code, out)
	}

	cancelled := report.Report{Err: errors.New("cancelled"), Silent: true}
	cliApp, out = newCLI()
	if code := cliApp.finish(context.Background(), run.Notifier{}, cancelled); code != 1 || !strings.Contains(out.String(), "FAILED: cancelled") {
		testState.Errorf("cancelled: exit=%d out=%q", code, out)
	}
}
