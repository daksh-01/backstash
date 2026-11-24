package run

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"backstash/internal/config"
	"backstash/internal/execx"
	"backstash/internal/secrets"
	"backstash/internal/storage"
	"backstash/internal/target"
)

// Standard Infisical key names. Targets may override their own password key in the YAML.
const (
	SecretS3KeyID     = "S3_ACCESS_KEY_ID"
	SecretS3Secret    = "S3_SECRET_ACCESS_KEY"
	SecretSlackToken  = "SLACK_BOT_TOKEN"
	SecretSMTPUser    = "SMTP_USER"
	SecretSMTPPass    = "SMTP_PASSWORD"
	sharedSecretsPath = "/backstash/shared"
)

// SecretPaths are the Infisical folders for an app: shared first, then the app (app wins).
func SecretPaths(cfg *config.Config, app *config.App) []string {
	shared := cfg.Host.SharedSecretsPath
	if shared == "" {
		shared = sharedSecretsPath
	}
	return []string{shared, app.SecretsPath}
}

// Open resolves secrets from Infisical and initializes Deps for app.
func Open(ctx context.Context, cfg *config.Config, app *config.App, configDir string, in io.Reader, out io.Writer, log *slog.Logger, runner execx.Runner) (*Deps, secrets.Set, error) {
	if runner == nil {
		runner = execx.Exec{}
	}
	var src secrets.Source
	var err error
	if cfg.Host.SecretsProvider == "aws" {
		src, err = secrets.AWSFromEnv(filepath.Join(configDir, "identity.env"))
	} else {
		src, err = secrets.FromEnv(filepath.Join(configDir, "identity.env"))
	}
	if err != nil {
		return nil, nil, err
	}
	sec, err := secrets.Resolve(ctx, src, SecretPaths(cfg, app)...)
	if err != nil {
		return nil, nil, err
	}
	deps := &Deps{
		Cfg: cfg,
		App: app,
		Log: log,
		In:  in,
		Out: out,
		Target: target.Deps{
			Run:        runner,
			Secrets:    sec,
			StagingDir: filepath.Join(cfg.Host.Staging(), app.Name+"-"+app.Environment), // per app: names like redis-<time>.rdb must not collide
		},
	}
	return deps, sec, nil
}

// NewStore builds the app's bucket client from the resolved secrets.
func NewStore(cfg *config.Config, app *config.App, sec secrets.Set) (storage.Store, error) {
	id, okID := sec.Get(SecretS3KeyID)
	key, okKey := sec.Get(SecretS3Secret)
	var missing []string
	if !okID {
		missing = append(missing, SecretS3KeyID)
	}
	if !okKey {
		missing = append(missing, SecretS3Secret)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("secrets not found in Infisical: %s", strings.Join(missing, ", "))
	}
	return storage.NewS3(storage.S3Config{
		Endpoint: cfg.Host.S3.Endpoint, Region: cfg.Host.S3.Region, Bucket: app.Bucket,
		AccessKeyID: id, SecretAccessKey: key,
	}), nil
}
