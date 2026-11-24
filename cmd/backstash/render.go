package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"backstash/internal/config"
	"backstash/internal/report"
	"backstash/internal/run"
)

// finish alerts, prints the outcome and returns the exit code.
func (client *cli) finish(ctx context.Context, notifier run.Notifier, rep report.Report) int {
	// A fresh context: Ctrl-C or SIGTERM cancels ctx, and that failure still has to be reported.
	notificationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	notifier.Send(notificationCtx, rep, client.capture.Text())

	if rep.Err != nil {
		fmt.Fprintln(client.out, "FAILED:", rep.Err)
	}
	for _, targetItem := range rep.Targets {
		switch {
		case targetItem.Err != nil:
			fmt.Fprintf(client.out, "%s: FAILED: %v\n", targetItem.Kind, targetItem.Err)
		case targetItem.Name != "":
			fmt.Fprintf(client.out, "%s: %s (%s)\n", targetItem.Kind, targetItem.Name, targetItem.Size.String())
		default:
			fmt.Fprintf(client.out, "%s: ok\n", targetItem.Kind)
		}
		if targetItem.Pruned > 0 {
			fmt.Fprintf(client.out, "  removed %d old backup(s)\n", targetItem.Pruned)
		}
		if targetItem.Note != "" {
			fmt.Fprintf(client.out, "  warning: %s\n", targetItem.Note)
		}
	}
	if rep.Failed() {
		return 1
	}
	return 0
}

func (client *cli) validate(ctx context.Context, appName string) int {
	cfg, err := config.Load(client.dir)
	if err != nil {
		fmt.Fprintln(client.out, "FAIL config:", err)
		return 1
	}
	fmt.Fprintf(client.out, "ok   config (%d app(s) in %s)\n", len(cfg.Apps), client.dir)

	var apps []*config.App
	if appName != "" {
		targetApp, err := cfg.Lookup(appName, client.env)
		if err != nil {
			fmt.Fprintln(client.out, "FAIL", err)
			return 1
		}
		apps = []*config.App{targetApp}
	} else {
		for _, key := range slices.Sorted(maps.Keys(cfg.Apps)) {
			apps = append(apps, cfg.Apps[key])
		}
	}

	failed := false
	for _, targetApp := range apps {
		fmt.Fprintf(client.out, "\n%s\n", targetApp.Key())
		deps, sec, err := client.open(ctx, cfg, targetApp)
		if err != nil {
			fmt.Fprintln(client.out, "FAIL secrets:", err)
			failed = true
			continue
		}
		store, err := run.NewStore(cfg, targetApp, sec)
		if err == nil {
			deps.Store = store
		}
		checks := deps.Validate(ctx, sec, nil)
		if err != nil {
			checks = append(checks, run.Check{Name: "bucket client", Err: err})
		}
		for _, checkItem := range checks {
			if checkItem.Err != nil {
				fmt.Fprintf(client.out, "FAIL %s: %v\n", checkItem.Name, checkItem.Err)
				failed = true
			} else {
				fmt.Fprintf(client.out, "ok   %s\n", checkItem.Name)
			}
		}
	}
	if failed {
		return 1
	}
	return 0
}
