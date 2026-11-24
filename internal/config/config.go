// Package config loads and validates the non-secret configuration: a host-wide
// config.yml plus one apps.d/<app>-<env>.yml per deployed app.
package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultEnvironment   = "prod"
	DefaultRetentionDays = 30
)

// DefaultDir is where config lives when --config-dir is not given: BACKSTASH_CONFIG_DIR, else
// ~/.config/backstash. Playbooks run without root, so /etc is not an option.
func DefaultDir() string {
	if dir := os.Getenv("BACKSTASH_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "backstash")
}

// Staging is the private local folder backups are written to before upload.
func (host Host) Staging() string {
	if host.StagingDir != "" {
		return host.StagingDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".local", "state", "backstash", "staging")
}

// Kind is one thing we can back up.
type Kind string

const (
	Postgres Kind = "postgres"
	Redis    Kind = "redis"
	Files    Kind = "files"
)

// AllKinds is the order targets run in.
var AllKinds = []Kind{Postgres, Redis, Files}

// IsValid reports whether kind is one of Postgres, Redis, or Files.
func (kind Kind) IsValid() bool {
	return slices.Contains(AllKinds, kind)
}

// Extension is the file suffix of a backup of this kind.
func (kind Kind) Extension() string {
	switch kind {
	case Postgres:
		return ".dump"
	case Redis:
		return ".rdb"
	default:
		return ".tar.gz"
	}
}

// ParseKind validates and converts a string into a typed Kind.
func ParseKind(rawKind string) (Kind, error) {
	kind := Kind(strings.TrimSpace(rawKind))
	if !kind.IsValid() {
		return "", fmt.Errorf("unknown target %q (use postgres, redis, files)", rawKind)
	}
	return kind, nil
}

// ParseKinds parses a comma-separated list of target kinds.
// An empty input returns (nil, nil), meaning all targets.
func ParseKinds(rawKinds string) ([]Kind, error) {
	if rawKinds == "" {
		return nil, nil
	}
	var out []Kind
	for _, part := range strings.Split(rawKinds, ",") {
		kind, err := ParseKind(part)
		if err != nil {
			return nil, err
		}
		out = append(out, kind)
	}
	return out, nil
}

// Mode is how a target is reached.
type Mode string

const (
	ModeDocker Mode = "docker" // exec inside a running container
	ModeTCP    Mode = "tcp"    // host or remote server over the network
	ModePath   Mode = "path"   // a directory on this host (files only)
)

var (
	slugRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	labelRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// Host is /etc/backstash/config.yml: defaults shared by every app on the machine.
type Host struct {
	S3                S3     `yaml:"s3"`
	Slack             Slack  `yaml:"slack"`
	SMTP              SMTP   `yaml:"smtp"`
	AlertEmailTo      string `yaml:"alert_email_to"`
	RetentionDays     *int   `yaml:"retention_days"`
	SharedSecretsPath string `yaml:"shared_secrets_path"`
	SecretsProvider   string `yaml:"secrets_provider"` // infisical (default) or aws
	StagingDir        string `yaml:"staging_dir"`
}

type S3 struct {
	Endpoint string `yaml:"endpoint"`
	Region   string `yaml:"region"`
}

type Slack struct {
	Channel string `yaml:"channel"`
}

type SMTP struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	TLS  *bool  `yaml:"tls"`
	From string `yaml:"from"`
}

// App is one apps.d file: one application in one environment.
type App struct {
	Name          string `yaml:"app"`
	Environment   string `yaml:"environment"`
	Bucket        string `yaml:"bucket"`
	RetentionDays *int   `yaml:"retention_days"`
	SecretsPath   string `yaml:"secrets_path"`
	SlackChannel  string `yaml:"slack_channel"`

	Postgres *PostgresTarget `yaml:"postgres"`
	Redis    *RedisTarget    `yaml:"redis"`
	Files    *FilesTarget    `yaml:"files"`

	// File is where this app was loaded from, for error messages.
	File string `yaml:"-"`
}

type PostgresTarget struct {
	Mode      Mode   `yaml:"mode"`
	Container string `yaml:"container"`
	Host      string `yaml:"host"`
	Port      int    `yaml:"port"`
	User      string `yaml:"user"`
	Database  string `yaml:"database"`
	// PasswordSecret names the secret holding the password (default POSTGRES_PASSWORD).
	PasswordSecret string `yaml:"password_secret"`
}

// PasswordKey is the secret holding the password (default POSTGRES_PASSWORD).
func (target *PostgresTarget) PasswordKey() string {
	return cmp.Or(target.PasswordSecret, "POSTGRES_PASSWORD")
}

type RedisTarget struct {
	Mode           Mode   `yaml:"mode"`
	Container      string `yaml:"container"`
	Host           string `yaml:"host"`
	Port           int    `yaml:"port"`
	PasswordSecret string `yaml:"password_secret"` // default REDIS_PASSWORD; optional if unset in Infisical
}

// PasswordKey is the secret holding the password (default REDIS_PASSWORD).
func (target *RedisTarget) PasswordKey() string {
	return cmp.Or(target.PasswordSecret, "REDIS_PASSWORD")
}

type FilesTarget struct {
	Mode      Mode   `yaml:"mode"`
	Container string `yaml:"container"`
	Path      string `yaml:"path"`
	Label     string `yaml:"label"`
}

// Config is everything loaded from one config directory.
type Config struct {
	Host Host
	Apps map[string]*App // keyed by "<app>/<env>"
}

// Key is how an app is addressed on the command line and in Apps.
func Key(app, env string) string { return app + "/" + env }

// Key returns this app's "<app>/<env>" key.
func (app *App) Key() string { return Key(app.Name, app.Environment) }

// Retention is the effective retention in days (app, then host, then 30). 0 disables pruning.
func (cfg *Config) Retention(app *App) int {
	switch {
	case app.RetentionDays != nil:
		return *app.RetentionDays
	case cfg.Host.RetentionDays != nil:
		return *cfg.Host.RetentionDays
	}
	return DefaultRetentionDays
}

// Prefix is the folder inside the app's bucket holding backups of kind k, ending in "/".
func (app *App) Prefix(kind Kind) string {
	return fmt.Sprintf("%s/%s/%s/", app.Name, app.Environment, kind)
}

// Kinds lists the targets this app configures, in run order.
func (app *App) Kinds() []Kind {
	var out []Kind
	if app.Postgres != nil {
		out = append(out, Postgres)
	}
	if app.Redis != nil {
		out = append(out, Redis)
	}
	if app.Files != nil {
		out = append(out, Files)
	}
	return out
}

// TargetLabel returns the filename label for backups of this kind.
func (app *App) TargetLabel(kind Kind) string {
	switch kind {
	case Postgres:
		if app.Postgres != nil {
			return app.Postgres.Database
		}
	case Redis:
		return "redis"
	case Files:
		if app.Files != nil {
			return app.Files.Label
		}
	}
	return ""
}

// Lookup finds an app by name and environment ("" means the default environment).
func (cfg *Config) Lookup(appName, env string) (*App, error) {
	if env == "" {
		env = DefaultEnvironment
	}
	if targetApp, ok := cfg.Apps[Key(appName, env)]; ok {
		return targetApp, nil
	}
	known := slices.Sorted(maps.Keys(cfg.Apps))
	return nil, fmt.Errorf("no app %q in environment %q (configured: %s)", appName, env, strings.Join(known, ", "))
}

// Load reads dir/config.yml and dir/apps.d/*.yml. Unknown keys are errors, and every
// problem found is reported at once rather than one per run.
func Load(dir string) (*Config, error) {
	cfg := &Config{Apps: map[string]*App{}}
	var errs []error

	hostPath := filepath.Join(dir, "config.yml")
	if err := decodeFile(hostPath, &cfg.Host); err != nil {
		return nil, err // nothing else makes sense without the host file
	}
	errs = append(errs, cfg.Host.validate(hostPath)...)

	// Accept both .yml and .yaml; sort after merging so load order is deterministic.
	ymlFiles, err := filepath.Glob(filepath.Join(dir, "apps.d", "*.yml"))
	if err != nil {
		return nil, err
	}
	yamlFiles, err := filepath.Glob(filepath.Join(dir, "apps.d", "*.yaml"))
	if err != nil {
		return nil, err
	}
	files := append(ymlFiles, yamlFiles...)
	slices.Sort(files)
	for _, filePath := range files {
		app := &App{File: filePath}
		if err := decodeFile(filePath, app); err != nil {
			errs = append(errs, err)
			continue
		}
		app.applyDefaults()
		errs = append(errs, app.validate()...)
		if prev, dup := cfg.Apps[app.Key()]; dup {
			errs = append(errs, fmt.Errorf("%s: %s is already defined in %s", filePath, app.Key(), prev.File))
			continue
		}
		cfg.Apps[app.Key()] = app
	}
	if len(files) == 0 {
		errs = append(errs, fmt.Errorf("%s: no app files found", filepath.Join(dir, "apps.d")))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

func decodeFile(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(into); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func (app *App) applyDefaults() {
	if app.Environment == "" {
		app.Environment = DefaultEnvironment
	}
	if postgresTarget := app.Postgres; postgresTarget != nil && postgresTarget.Port == 0 {
		postgresTarget.Port = 5432
	}
	if redisTarget := app.Redis; redisTarget != nil && redisTarget.Port == 0 {
		redisTarget.Port = 6379
	}
	if filesTarget := app.Files; filesTarget != nil && filesTarget.Label == "" {
		filesTarget.Label = "files"
	}
}

// problems collects validation errors for one file, so every mistake is reported at once.
type problems struct {
	file string
	errs []error
}

func (probs *problems) addf(format string, args ...any) {
	probs.errs = append(probs.errs, fmt.Errorf("%s: %s", probs.file, fmt.Sprintf(format, args...)))
}

func (host *Host) validate(path string) []error {
	probs := problems{file: path}
	if host.RetentionDays != nil && *host.RetentionDays < 0 {
		probs.addf("retention_days must be >= 0")
	}
	if host.SMTP.Host != "" && host.AlertEmailTo == "" {
		probs.addf("smtp.host is set but alert_email_to is not")
	}
	if host.SharedSecretsPath != "" && !strings.HasPrefix(host.SharedSecretsPath, "/") {
		probs.addf("shared_secrets_path must start with '/' (got %q)", host.SharedSecretsPath)
	}
	if p := host.SecretsProvider; p != "" && p != "infisical" && p != "aws" {
		probs.addf("secrets_provider must be infisical or aws (got %q)", p)
	}
	return probs.errs
}

func (app *App) validate() []error {
	probs := problems{file: app.File}
	if !slugRe.MatchString(app.Name) {
		probs.addf("app %q must be lowercase letters, digits and dashes", app.Name)
	}
	if !slugRe.MatchString(app.Environment) {
		probs.addf("environment %q must be lowercase letters, digits and dashes", app.Environment)
	}
	if app.Bucket == "" {
		probs.addf("bucket is required")
	}
	if app.SecretsPath == "" {
		probs.addf("secrets_path is required")
	} else if !strings.HasPrefix(app.SecretsPath, "/") {
		probs.addf("secrets_path must start with '/' (got %q)", app.SecretsPath)
	}
	if app.RetentionDays != nil && *app.RetentionDays < 0 {
		probs.addf("retention_days must be >= 0")
	}
	if len(app.Kinds()) == 0 {
		probs.addf("configure at least one of postgres, redis, files")
	}

	if postgresTarget := app.Postgres; postgresTarget != nil {
		if postgresTarget.User == "" || postgresTarget.Database == "" {
			probs.addf("postgres.user and postgres.database are required")
		}
		validateConn(&probs, "postgres", postgresTarget.Mode, postgresTarget.Container, postgresTarget.Host, postgresTarget.Port)
	}
	if redisTarget := app.Redis; redisTarget != nil {
		validateConn(&probs, "redis", redisTarget.Mode, redisTarget.Container, redisTarget.Host, redisTarget.Port)
	}
	if filesTarget := app.Files; filesTarget != nil {
		switch filesTarget.Mode {
		case ModeDocker:
			if filesTarget.Container == "" {
				probs.addf("files.container is required when mode is docker")
			}
		case ModePath:
		default:
			probs.addf("files.mode must be docker or path, got %q", filesTarget.Mode)
		}
		if !filepath.IsAbs(filesTarget.Path) {
			probs.addf("files.path must be an absolute path")
		}
		if !labelRe.MatchString(filesTarget.Label) {
			probs.addf("files.label %q may only use letters, digits, _ . -", filesTarget.Label)
		}
	}
	return probs.errs
}

func validateConn(probs *problems, what string, mode Mode, container, host string, port int) {
	switch mode {
	case ModeDocker:
		if container == "" {
			probs.addf("%s.container is required when mode is docker", what)
		}
	case ModeTCP:
		if host == "" {
			probs.addf("%s.host is required when mode is tcp", what)
		}
		if port <= 0 || port > 65535 {
			probs.addf("%s.port must be between 1 and 65535, got %d", what, port)
		}
	default:
		probs.addf("%s.mode must be docker or tcp, got %q", what, mode)
	}
}
