// Package secrets resolves secret values from Infisical at run time, so no per-app
// .env file with passwords has to exist on the host.
package secrets

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultURL = "https://app.infisical.com"
	defaultEnv = "prod"
	timeout    = 15 * time.Second
)

// Set is a bag of secrets. Its String form never prints values.
type Set map[string]string

func (secretsSet Set) String() string { return fmt.Sprintf("secrets(%d)", len(secretsSet)) }

// Get returns the secret and whether it was present and non-empty.
func (secretsSet Set) Get(name string) (string, bool) {
	val, ok := secretsSet[name]
	return val, ok && val != ""
}

// Source reads every secret in one Infisical folder.
type Source interface {
	Read(ctx context.Context, path string) (map[string]string, error)
}

// Resolve reads each path in order; a later path wins on a clash (shared first, app last).
func Resolve(ctx context.Context, src Source, paths ...string) (Set, error) {
	out := Set{}
	for _, secretPath := range paths {
		if secretPath == "" {
			continue
		}
		got, err := src.Read(ctx, secretPath)
		if err != nil {
			return nil, fmt.Errorf("read secrets from %s: %w", secretPath, err)
		}
		for key, val := range got {
			out[key] = val
		}
	}
	return out, nil
}

// Infisical talks to the Infisical REST API with a universal-auth machine identity.
type Infisical struct {
	URL, ClientID, ClientSecret, ProjectID, Env string
	HTTP                                        *http.Client
	Retries                                     int
	Sleep                                       func(time.Duration)

	token string
}

type loginRequest struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

type loginResponse struct {
	AccessToken string `json:"accessToken"`
}

type secretItem struct {
	Key   string `json:"secretKey"`
	Value string `json:"secretValue"`
}

type secretsResponse struct {
	Secrets []secretItem `json:"secrets"`
}

// FromEnv builds a client from INFISICAL_* variables. If INFISICAL_UNIVERSAL_AUTH_CLIENT_ID
// is not in the environment, it loads identityFile (KEY=VALUE lines) without mutating the
// process environment. Environment variables always win over the file.
func FromEnv(identityFile string) (*Infisical, error) {
	var fileVars map[string]string
	if identityFile != "" {
		vars, err := ReadEnvFile(identityFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		fileVars = vars
	}
	lookup := func(name string) string {
		if val := os.Getenv(name); val != "" {
			return val
		}
		if fileVars != nil {
			return fileVars[name]
		}
		return ""
	}
	lookupOr := func(name, fallback string) string {
		if val := lookup(name); val != "" {
			return val
		}
		return fallback
	}

	client := &Infisical{
		URL:          lookupOr("INFISICAL_URL", defaultURL),
		ClientID:     lookup("INFISICAL_UNIVERSAL_AUTH_CLIENT_ID"),
		ClientSecret: lookup("INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET"),
		ProjectID:    lookup("INFISICAL_PROJECT_ID"),
		Env:          lookupOr("INFISICAL_ENV", defaultEnv),
		HTTP:         &http.Client{Timeout: timeout},
		Retries:      2,
		Sleep:        time.Sleep,
	}
	var missing []string
	for _, need := range []struct{ name, value string }{
		{"INFISICAL_UNIVERSAL_AUTH_CLIENT_ID", client.ClientID},
		{"INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET", client.ClientSecret},
		{"INFISICAL_PROJECT_ID", client.ProjectID},
	} {
		if need.value == "" {
			missing = append(missing, need.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("infisical identity incomplete, missing: %s", strings.Join(missing, ", "))
	}
	return client, nil
}

// ReadEnvFile reads variables from a KEY=VALUE file into a map without mutating process environment.
func ReadEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	vars := make(map[string]string)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		vars[key] = val
	}
	return vars, scanner.Err()
}

func (client *Infisical) login(ctx context.Context) error {
	body, err := json.Marshal(loginRequest{ClientID: client.ClientID, ClientSecret: client.ClientSecret})
	if err != nil {
		return fmt.Errorf("infisical login: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		client.URL+"/api/v1/auth/universal-auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	var resp loginResponse
	if err := client.do(req, &resp); err != nil {
		return fmt.Errorf("infisical login: %w", err)
	}
	if resp.AccessToken == "" {
		return errors.New("infisical login: empty access token")
	}
	client.token = resp.AccessToken
	return nil
}

// Read implements Source, logging in on first use.
func (client *Infisical) Read(ctx context.Context, path string) (map[string]string, error) {
	if client.token == "" {
		if err := client.login(ctx); err != nil {
			return nil, err
		}
	}
	query := url.Values{
		"workspaceId": {client.ProjectID},
		"environment": {client.Env},
		"secretPath":  {path},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, client.URL+"/api/v3/secrets/raw?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+client.token)
	var resp secretsResponse
	if err := client.do(req, &resp); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(resp.Secrets))
	for _, secret := range resp.Secrets {
		out[secret.Key] = secret.Value
	}
	return out, nil
}

func (client *Infisical) sleepFn() func(time.Duration) {
	if client.Sleep != nil {
		return client.Sleep
	}
	return time.Sleep
}

// do executes req and decodes the JSON response body into into.
// Retries on network errors, rate limits (HTTP 429), and 5xx server errors.
func (client *Infisical) do(req *http.Request, into any) error {
	sleep := client.sleepFn()
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return err
		}
		_ = req.Body.Close()
	}

	for attempt := 0; ; attempt++ {
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
		res, err := client.HTTP.Do(req)
		var retry bool
		var retryAfter time.Duration
		var apiErr error

		if err != nil {
			cause := errors.Unwrap(err)
			if cause == nil {
				cause = err
			}
			apiErr = fmt.Errorf("request failed: %w", cause)
			retry = true
		} else {
			raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
			_ = res.Body.Close()

			if res.StatusCode == http.StatusOK {
				return json.Unmarshal(raw, into)
			}
			if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
				retry = true
				if res.StatusCode == http.StatusTooManyRequests {
					if rHeader := res.Header.Get("Retry-After"); rHeader != "" {
						if s, parseErr := strconv.Atoi(rHeader); parseErr == nil && s > 0 {
							retryAfter = time.Duration(s) * time.Second
						}
					}
				}
			}
			apiErr = fmt.Errorf("HTTP %d: %s", res.StatusCode, truncate(string(raw), 200))
		}

		if !retry || attempt >= client.Retries {
			return apiErr
		}
		delay := time.Duration(attempt+1) * 200 * time.Millisecond
		if retryAfter > delay {
			delay = retryAfter
		}
		sleep(delay)
	}
}

// truncate shortens text to at most maxLen bytes so that large API error bodies do
// not flood logs.
func truncate(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}
