package run

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	"backstash/internal/config"
	"backstash/internal/execx"
	"backstash/internal/secrets"
)

// Check is one line of `backstash validate`.
type Check struct {
	Name string
	Err  error
}

// requiredSecrets are the secrets this app cannot run without.
func (deps *Deps) requiredSecrets() []string {
	names := []string{SecretS3KeyID, SecretS3Secret}
	if postgresTarget := deps.App.Postgres; postgresTarget != nil && postgresTarget.Mode == config.ModeTCP {
		names = append(names, postgresTarget.PasswordKey())
	}
	return names
}

// requiredTools returns the host binaries the app needs, deduplicated and sorted.
// Docker targets only need "docker"; tcp/path targets need their own client tool.
func (deps *Deps) requiredTools() []string {
	var tools []string
	need := func(mode config.Mode, hostTools ...string) {
		if mode == config.ModeDocker {
			tools = append(tools, "docker")
		} else {
			tools = append(tools, hostTools...)
		}
	}
	if postgresTarget := deps.App.Postgres; postgresTarget != nil {
		need(postgresTarget.Mode, "pg_dump", "pg_restore")
	}
	if redisTarget := deps.App.Redis; redisTarget != nil {
		need(redisTarget.Mode, "redis-cli")
	}
	if filesTarget := deps.App.Files; filesTarget != nil {
		need(filesTarget.Mode, "tar")
	}
	slices.Sort(tools)
	return slices.Compact(tools)
}

// containers returns the distinct container names used by this app's docker-mode
// targets, sorted alphabetically. Used by Validate to check each container is running.
func (deps *Deps) containers() []string {
	seen := map[string]bool{}
	if postgresTarget := deps.App.Postgres; postgresTarget != nil && postgresTarget.Mode == config.ModeDocker {
		seen[postgresTarget.Container] = true
	}
	if redisTarget := deps.App.Redis; redisTarget != nil && redisTarget.Mode == config.ModeDocker {
		seen[redisTarget.Container] = true
	}
	if filesTarget := deps.App.Files; filesTarget != nil && filesTarget.Mode == config.ModeDocker {
		seen[filesTarget.Container] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

// containerRunning asks docker; a wrong container name should fail at setup, not at 2am.
func (deps *Deps) containerRunning(ctx context.Context, name string) error {
	var out bytes.Buffer
	err := deps.Target.Run.Run(ctx, execx.Cmd{
		Name: "docker", Args: []string{"inspect", "-f", "{{.State.Running}}", name}, Stdout: &out,
	})
	if err == nil && strings.TrimSpace(out.String()) != "true" {
		err = errors.New("container exists but is not running")
	}
	return err
}

// Validate checks everything a backup needs without writing anything to the bucket:
// secrets present, tools installed for the configured modes, containers running, bucket reachable.
func (deps *Deps) Validate(ctx context.Context, sec secrets.Set, lookPath func(string) (string, error)) []Check {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	var out []Check
	add := func(name string, err error) { out = append(out, Check{name, err}) }

	for _, name := range deps.requiredSecrets() {
		var err error
		if _, ok := sec.Get(name); !ok {
			err = errors.New("not found in Infisical")
		}
		add("secret "+name, err)
	}
	for _, tool := range deps.requiredTools() {
		_, err := lookPath(tool)
		add("tool "+tool, err)
	}
	if _, err := lookPath("docker"); err == nil && deps.Target.Run != nil {
		for _, name := range deps.containers() {
			add("container "+name, deps.containerRunning(ctx, name))
		}
	}
	if deps.Store != nil {
		_, err := deps.Store.List(ctx, deps.App.Name+"/"+deps.App.Environment+"/")
		add("bucket "+deps.App.Bucket, err)
	}
	if err := os.MkdirAll(deps.Target.StagingDir, 0o700); err != nil {
		add("staging dir", err)
	} else {
		add("staging dir "+deps.Target.StagingDir, nil)
	}
	return out
}
