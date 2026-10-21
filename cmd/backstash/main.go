// Command backstash backs up and restores Postgres, Redis and app files to S3-compatible storage.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"backstash/internal/config"
	"backstash/internal/runlog"
)

// version is set at release time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usage = `backstash: back up Postgres, Redis and app files to S3-compatible storage.

Usage:
  backstash [--config-dir DIR] <command> [args]

Commands:
  backup   <app> [--env prod] [--only postgres,redis,files] [--keep]
  restore  <app> <postgres|redis|files> [NAME] [--env prod] [--yes]
  list     <app> [--env prod] [--only ...]
  prune    <app> [--env prod]
  validate [app] [--env prod]     check config, secrets, tools and the bucket; writes nothing
  version

Config lives in --config-dir (default $BACKSTASH_CONFIG_DIR, else ~/.config/backstash):
config.yml, apps.d/<app>-<env>.yml, identity.env (Infisical machine identity).
`

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// parseInterleaved parses flags that may come before, between or after positional arguments
// (Go's flag package stops at the first positional).
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// options is everything after the command name: its flags and positional arguments.
type options struct {
	env       string
	only      []config.Kind
	keep, yes bool
	pos       []string
}

func (opts options) arg(index int) string {
	if index < len(opts.pos) {
		return opts.pos[index]
	}
	return ""
}

func realMain(args []string, in io.Reader, out, errw io.Writer) (code int) {
	defer func() {
		if panicVal := recover(); panicVal != nil {
			fmt.Fprintf(errw, "FAILED: unexpected error: %v\n", panicVal)
			code = 1
		}
	}()

	global := flag.NewFlagSet("backstash", flag.ContinueOnError)
	global.SetOutput(errw)
	global.Usage = func() { fmt.Fprint(errw, usage) }
	dir := global.String("config-dir", config.DefaultDir(), "config directory")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if global.NArg() == 0 {
		fmt.Fprint(errw, usage)
		return 2
	}
	cmd, rest := global.Arg(0), global.Args()[1:]

	switch cmd {
	case "version":
		fmt.Fprintln(out, "backstash", version)
		return 0
	case "help":
		fmt.Fprint(out, usage)
		return 0
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(errw)
	opts := options{}
	fs.StringVar(&opts.env, "env", config.DefaultEnvironment, "environment")
	only := fs.String("only", "", "comma-separated targets")
	fs.BoolVar(&opts.keep, "keep", false, "backup: keep the local copy after upload")
	fs.BoolVar(&opts.yes, "yes", false, "restore: skip the confirmation prompt")
	var err error
	if opts.pos, err = parseInterleaved(fs, rest); err != nil {
		return 2
	}
	if opts.only, err = config.ParseKinds(*only); err != nil {
		fmt.Fprintln(errw, "error:", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	client := &cli{dir: *dir, env: opts.env, in: in, out: out, errw: errw, capture: runlog.New(errw)}
	return client.dispatch(ctx, cmd, opts)
}
