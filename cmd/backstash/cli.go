package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"backstash/internal/config"
	"backstash/internal/report"
	"backstash/internal/run"
	"backstash/internal/runlog"
	"backstash/internal/secrets"
)

type cli struct {
	dir, env string
	in       io.Reader
	out      io.Writer
	errw     io.Writer
	capture  *runlog.Capture
}

// dispatch runs one command. Exit codes: 0 success, 1 a run or setup step failed, 2 bad arguments.
func (client *cli) dispatch(ctx context.Context, cmd string, opts options) int {
	switch cmd {
	case "validate":
		return client.validate(ctx, opts.arg(0))
	case "backup":
		return client.withOneApp(ctx, cmd, opts, func(deps *run.Deps, notifier run.Notifier) int {
			return client.finish(ctx, notifier, deps.Backup(ctx, run.BackupOpts{Only: opts.only, Keep: opts.keep}))
		})
	case "prune":
		return client.withOneApp(ctx, cmd, opts, func(deps *run.Deps, notifier run.Notifier) int {
			return client.finish(ctx, notifier, deps.Prune(ctx))
		})
	case "list":
		return client.withOneApp(ctx, cmd, opts, func(deps *run.Deps, _ run.Notifier) int {
			if err := deps.List(ctx, opts.only); err != nil {
				fmt.Fprintln(client.errw, "FAILED:", err)
				return 1
			}
			return 0
		})
	case "restore":
		// restore needs two positional args (app and kind) so it handles its own
		// argument validation instead of going through withOneApp.
		return client.restore(ctx, opts)
	}
	fmt.Fprintf(client.errw, "unknown command %q\n\n%s", cmd, usage)
	return 2
}

// startupAction names the alert for a startup failure; "list" is read-only and never alerts.
var startupAction = map[string]report.Action{
	"backup": report.ActionBackup, "prune": report.ActionPrune, "restore": report.ActionRestore,
}

// withOneApp is for commands taking exactly one <app> argument.
func (client *cli) withOneApp(ctx context.Context, cmd string, opts options, fn func(*run.Deps, run.Notifier) int) int {
	if len(opts.pos) != 1 {
		fmt.Fprintf(client.errw, "usage: backstash %s <app> [flags]\n", cmd)
		return 2
	}
	return client.withApp(ctx, opts.pos[0], startupAction[cmd], fn)
}

func (client *cli) restore(ctx context.Context, opts options) int {
	if len(opts.pos) < 2 || len(opts.pos) > 3 {
		fmt.Fprintln(client.errw, "usage: backstash restore <app> <postgres|redis|files> [NAME] [--yes]")
		return 2
	}
	kind, err := config.ParseKind(opts.pos[1])
	if err != nil {
		fmt.Fprintln(client.errw, "error: restore target must be one of postgres, redis, files")
		return 2
	}
	return client.withApp(ctx, opts.pos[0], startupAction["restore"], func(deps *run.Deps, notifier run.Notifier) int {
		return client.finish(ctx, notifier, deps.Restore(ctx, kind, opts.arg(2), opts.yes))
	})
}

// open loads config and secrets and builds the dependencies for one app.
func (client *cli) open(ctx context.Context, cfg *config.Config, app *config.App) (*run.Deps, secrets.Set, error) {
	return run.Open(ctx, cfg, app, client.dir, client.in, client.out, client.capture.Logger(), nil)
}

// fail prints err to the error writer and returns exit code 1.
func (client *cli) fail(err error) int {
	fmt.Fprintln(client.errw, "FAILED:", err)
	return 1
}

// withApp does everything a command needs before it can run. If secret resolution or store
// creation fails, it attempts to notify using fallback credentials if present before failing.
func (client *cli) withApp(ctx context.Context, appName string, action report.Action, fn func(*run.Deps, run.Notifier) int) int {
	cfg, err := config.Load(client.dir)
	if err != nil {
		return client.fail(err)
	}
	app, err := cfg.Lookup(appName, client.env)
	if err != nil {
		return client.fail(err)
	}
	deps, sec, err := client.open(ctx, cfg, app)
	if err != nil {
		client.notifyStartupFailure(ctx, cfg, app, action, err)
		return client.fail(err)
	}
	deps.Store, err = run.NewStore(cfg, app, sec)
	if err != nil {
		client.notifyStartupFailure(ctx, cfg, app, action, err)
		return client.fail(err)
	}
	return fn(deps, run.NewNotifier(cfg, app, sec, deps.Log))
}

func (client *cli) notifyStartupFailure(ctx context.Context, cfg *config.Config, app *config.App, action report.Action, err error) {
	if action == "" {
		return
	}
	fallbackSec := secrets.Set{}
	for _, key := range []string{run.SecretSlackToken, run.SecretSMTPUser, run.SecretSMTPPass} {
		if val := os.Getenv(key); val != "" {
			fallbackSec[key] = val
		}
	}
	if envMap, readErr := secrets.ReadEnvFile(filepath.Join(client.dir, "identity.env")); readErr == nil {
		for _, key := range []string{run.SecretSlackToken, run.SecretSMTPUser, run.SecretSMTPPass} {
			if val, ok := envMap[key]; ok && fallbackSec[key] == "" {
				fallbackSec[key] = val
			}
		}
	}
	notifier := run.NewNotifier(cfg, app, fallbackSec, client.capture.Logger())
	rep := report.New(action, app.Name, app.Environment)
	rep.Err = fmt.Errorf("startup failed: %w", err)
	notificationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	notifier.Send(notificationCtx, rep, client.capture.Text())
}
