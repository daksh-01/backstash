package target

import (
	"context"
	"io"
	"os"
	"strconv"

	"backstash/internal/config"
	"backstash/internal/execx"
)

// Fail fast on restore instead of hanging when the running app holds locks.
const restoreLockTimeout = "-c lock_timeout=30s"

// pgCmd builds the argument list and environment for a pg_dump or pg_restore invocation.
// In docker mode the host/port flags are omitted because the socket inside the official
// Postgres image trusts local connections. The password is passed via the environment
// (never in argv) and only when it is non-empty, so socket-trust containers are not broken
// by an empty PGPASSWORD.
func pgCmd(target *config.PostgresTarget, password, tool string, args ...string) execx.Cmd {
	conn := []string{"-h", target.Host, "-p", strconv.Itoa(target.Port), "-U", target.User}
	if target.Mode == config.ModeDocker { // inside the container: local socket, no host/port
		conn = []string{"-U", target.User}
	}
	cmd := execx.Cmd{Name: tool, Args: append(conn, args...)}
	if password != "" {
		cmd.Env = []string{"PGPASSWORD=" + password}
	}
	return cmd
}

// pgPassword is required for tcp (the server checks it) and optional for docker, where the socket
// inside the official image trusts local connections.
func (deps Deps) pgPassword(target *config.PostgresTarget) (string, error) {
	password, ok := deps.Secrets.Get(target.PasswordKey())
	if !ok && target.Mode == config.ModeTCP {
		return "", ErrNoSecret{target.PasswordKey()}
	}
	return password, nil
}

// DumpPostgres writes a pg_dump custom-format (compressed) backup to the staging dir.
func (deps Deps) DumpPostgres(ctx context.Context, target *config.PostgresTarget) (Artifact, error) {
	password, err := deps.pgPassword(target)
	if err != nil {
		return Artifact{}, err
	}
	name := BackupName(target.Database, config.Postgres.Extension(), deps.now())
	return deps.capture(name, func(out io.Writer) error {
		cmd := pgCmd(target, password, "pg_dump", "-Fc", target.Database)
		cmd.Stdout = out
		return deps.Run.Run(ctx, place(target.Mode, target.Container, cmd))
	})
}

// RestorePostgres loads a custom-format dump in one transaction: if anything fails, including
// a lock timeout because the app is still connected, nothing is applied.
func (deps Deps) RestorePostgres(ctx context.Context, target *config.PostgresTarget, path string) error {
	password, err := deps.pgPassword(target)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	cmd := pgCmd(target, password, "pg_restore", "-d", target.Database, "--clean", "--if-exists", "--no-owner", "--single-transaction")
	cmd.Env = append(cmd.Env, "PGOPTIONS="+restoreLockTimeout)
	cmd.Stdin = file
	if err := deps.Run.Run(ctx, place(target.Mode, target.Container, cmd)); err != nil {
		return &RestoreError{Err: err, Note: "The restore runs in one transaction, so the database should be unchanged."}
	}
	return nil
}

// RestoreError wraps a failed restore with what state the target was left in.
type RestoreError struct {
	Err  error
	Note string
}

func (err *RestoreError) Error() string { return err.Err.Error() + " (" + err.Note + ")" }
func (err *RestoreError) Unwrap() error { return err.Err }
