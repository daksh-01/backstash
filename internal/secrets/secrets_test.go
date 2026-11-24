package secrets

import (
	"context"
	"encoding/json"
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
	if _, err := AWSFromEnv(""); err == nil || !strings.Contains(err.Error(), "AWS_REGION") {
		testState.Fatalf("got %v", err)
	}
}
