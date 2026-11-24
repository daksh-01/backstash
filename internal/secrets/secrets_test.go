package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeInfisical(testState *testing.T) *httptest.Server {
	testState.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/universal-auth/login", func(respWriter http.ResponseWriter, req *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(req.Body).Decode(&in)
		if in["clientId"] != "id" || in["clientSecret"] != "sec" {
			http.Error(respWriter, `{"message":"bad creds"}`, http.StatusUnauthorized)
			return
		}
		_, _ = respWriter.Write([]byte(`{"accessToken":"tok"}`))
	})
	mux.HandleFunc("/api/v3/secrets/raw", func(respWriter http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer tok" {
			http.Error(respWriter, "unauthorized", http.StatusUnauthorized)
			return
		}
		query := req.URL.Query()
		if query.Get("workspaceId") != "proj" || query.Get("environment") != "prod" {
			http.Error(respWriter, "wrong scope", http.StatusBadRequest)
			return
		}
		switch query.Get("secretPath") {
		case "/shared":
			_, _ = respWriter.Write([]byte(`{"secrets":[{"secretKey":"SLACK_BOT_TOKEN","secretValue":"xoxb"},{"secretKey":"X","secretValue":"shared"}]}`))
		case "/app":
			_, _ = respWriter.Write([]byte(`{"secrets":[{"secretKey":"X","secretValue":"app"}]}`))
		default:
			http.NotFound(respWriter, req)
		}
	})
	srv := httptest.NewServer(mux)
	testState.Cleanup(srv.Close)
	return srv
}

func client(targetURL string) *Infisical {
	return &Infisical{URL: targetURL, ClientID: "id", ClientSecret: "sec", ProjectID: "proj", Env: "prod", HTTP: http.DefaultClient}
}

func TestResolveAppWinsOverShared(testState *testing.T) {
	srv := fakeInfisical(testState)
	got, err := Resolve(context.Background(), client(srv.URL), "/shared", "/app")
	if err != nil {
		testState.Fatal(err)
	}
	if val, _ := got.Get("X"); val != "app" {
		testState.Errorf("X = %q, want app", val)
	}
	if val, ok := got.Get("SLACK_BOT_TOKEN"); !ok || val != "xoxb" {
		testState.Errorf("shared secret missing")
	}
}

func TestBadCredentials(testState *testing.T) {
	srv := fakeInfisical(testState)
	clientInstance := client(srv.URL)
	clientInstance.ClientSecret = "wrong"
	if _, err := Resolve(context.Background(), clientInstance, "/shared"); err == nil || !strings.Contains(err.Error(), "401") {
		testState.Fatalf("got %v", err)
	}
}

func TestUnknownPathNamesThePath(testState *testing.T) {
	srv := fakeInfisical(testState)
	_, err := Resolve(context.Background(), client(srv.URL), "/missing")
	if err == nil || !strings.Contains(err.Error(), "/missing") {
		testState.Fatalf("got %v", err)
	}
}

func TestSetStringHidesValues(testState *testing.T) {
	if secretsText := (Set{"A": "hunter2"}).String(); strings.Contains(secretsText, "hunter2") {
		testState.Errorf("leaked: %s", secretsText)
	}
}

func TestInfisicalRetriesRateLimitAndRecovers(testState *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(respWriter http.ResponseWriter, req *http.Request) {
		attempts++
		if attempts == 1 {
			respWriter.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = respWriter.Write([]byte(`{"accessToken":"recovered-token"}`))
	}))
	defer srv.Close()

	c := client(srv.URL)
	c.Retries = 2
	c.Sleep = func(time.Duration) {}
	if err := c.login(context.Background()); err != nil || attempts != 2 {
		testState.Fatalf("login failed: attempts=%d, err=%v", attempts, err)
	}
	if c.token != "recovered-token" {
		testState.Errorf("token = %q, want recovered-token", c.token)
	}
}

func TestFromEnvLoadsIdentityFile(testState *testing.T) {
	for _, envKey := range []string{"INFISICAL_UNIVERSAL_AUTH_CLIENT_ID", "INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET", "INFISICAL_PROJECT_ID", "INFISICAL_ENV", "INFISICAL_URL"} {
		testState.Setenv(envKey, "")
		os.Unsetenv(envKey)
	}
	envFile := filepath.Join(testState.TempDir(), "identity.env")
	body := "# comment\nINFISICAL_UNIVERSAL_AUTH_CLIENT_ID=id\nexport INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET=\"sec\"\nINFISICAL_PROJECT_ID=proj\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		testState.Fatal(err)
	}
	clientInstance, err := FromEnv(envFile)
	if err != nil {
		testState.Fatal(err)
	}
	if clientInstance.ClientID != "id" || clientInstance.ClientSecret != "sec" || clientInstance.ProjectID != "proj" || clientInstance.Env != "prod" {
		testState.Errorf("got %+v", clientInstance)
	}
}

func TestFromEnvListsMissing(testState *testing.T) {
	for _, envKey := range []string{"INFISICAL_UNIVERSAL_AUTH_CLIENT_ID", "INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET", "INFISICAL_PROJECT_ID"} {
		testState.Setenv(envKey, "")
	}
	_, err := FromEnv("/nonexistent/identity.env")
	if err == nil || !strings.Contains(err.Error(), "INFISICAL_PROJECT_ID") {
		testState.Fatalf("got %v", err)
	}
}

func TestAWSReadsJSONSecret(testState *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_, _ = w.Write([]byte(`{"Name":"/backstash/shared","SecretString":"{\"S3_ACCESS_KEY_ID\":\"abc\"}"}`))
	}))
	defer srv.Close()
	testState.Setenv("AWS_ACCESS_KEY_ID", "k")
	testState.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	testState.Setenv("AWS_REGION", "us-east-1")
	testState.Setenv("AWS_ENDPOINT_URL", srv.URL)
	src, err := AWSFromEnv("")
	if err != nil {
		testState.Fatal(err)
	}
	set, err := Resolve(context.Background(), src, "/backstash/shared")
	if err != nil {
		testState.Fatal(err)
	}
	if val, ok := set.Get("S3_ACCESS_KEY_ID"); !ok || val != "abc" {
		testState.Fatalf("got %v", set)
	}
}

func TestAWSFromEnvReportsMissing(testState *testing.T) {
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_REGION"} {
		testState.Setenv(name, "")
	}
	testState.Setenv("AWS_CONFIG_FILE", "/dev/null")
	testState.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	if _, err := AWSFromEnv(""); err == nil || !strings.Contains(err.Error(), "AWS_REGION") {
		testState.Fatalf("got %v", err)
	}
}

type flaky struct {
	vals map[string]string
	down bool
}

func (src *flaky) CacheKey() string { return "client-secret" }
func (src *flaky) Read(context.Context, string) (map[string]string, error) {
	if src.down {
		return nil, errors.New("provider down")
	}
	return src.vals, nil
}

func TestCacheServesLastGoodSecretsWhileProviderIsDown(testState *testing.T) {
	path := filepath.Join(testState.TempDir(), "c")
	src := &flaky{vals: map[string]string{"A": "1"}}
	if _, from, err := ResolveCached(context.Background(), src, path, time.Hour, "/p"); err != nil || from {
		testState.Fatalf("live read: from=%v err=%v", from, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		testState.Fatalf("cache mode %v", info.Mode())
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), `"A"`) {
		testState.Fatal("cache is not sealed")
	}
	src.down = true
	set, from, err := ResolveCached(context.Background(), src, path, time.Hour, "/p")
	if err != nil || !from || set["A"] != "1" {
		testState.Fatalf("cached read: %v from=%v err=%v", set, from, err)
	}
	// different folders, an expired copy, and caching off must all surface the outage
	if _, _, err := ResolveCached(context.Background(), src, path, time.Hour, "/other"); err == nil {
		testState.Fatal("served a cache for different paths")
	}
	if _, _, err := ResolveCached(context.Background(), src, path, time.Nanosecond, "/p"); err == nil {
		testState.Fatal("served an expired cache")
	}
	if _, _, err := ResolveCached(context.Background(), src, path, 0, "/p"); err == nil {
		testState.Fatal("cache used while disabled")
	}
}
