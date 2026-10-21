package target

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"backstash/internal/config"
	"backstash/internal/execx"
)

const containerRDB = "/tmp/backstash-dump.rdb"

// DumpRedis takes a consistent RDB snapshot with `redis-cli --rdb`, without touching the
// server's own data dir. In docker mode the snapshot is written inside the container, copied
// out with `docker cp`, and removed again (older redis-cli cannot stream an RDB to stdout).
func (deps Deps) DumpRedis(ctx context.Context, target *config.RedisTarget) (Artifact, error) {
	var env []string
	if password, ok := deps.Secrets.Get(target.PasswordKey()); ok {
		env = []string{"REDISCLI_AUTH=" + password}
	}

	name := BackupName("redis", config.Redis.Extension(), deps.now())
	file, path, err := deps.reserve(name)
	if err != nil {
		return Artifact{}, err
	}
	_ = file.Close()

	if target.Mode == config.ModeDocker {
		err = deps.redisViaDocker(ctx, target, env, path)
	} else {
		err = deps.Run.Run(ctx, execx.Cmd{
			Name: "redis-cli", Env: env,
			Args: []string{"-h", target.Host, "-p", strconv.Itoa(target.Port), "--rdb", path},
		})
	}
	if err == nil {
		err = os.Chmod(path, fileMode) // redis-cli may have replaced the file with its own mode
	}
	return finish(path, name, err)
}

// redisViaDocker triggers an RDB snapshot inside the container, copies the file out,
// then removes it. Older redis-cli cannot stream an RDB to stdout, so the file must
// land inside the container first.
//
// Cleanup uses a fresh context so the rm -f runs even when the caller's context has
// been cancelled (e.g. Ctrl-C or a timeout).
func (deps Deps) redisViaDocker(ctx context.Context, target *config.RedisTarget, env []string, dest string) error {
	// Schedule cleanup before anything else so the container file is always removed,
	// even if the dump or copy step fails.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		rmCmd := execx.Cmd{Name: "rm", Args: []string{"-f", containerRDB}}
		_ = deps.Run.Run(cleanupCtx, execx.InContainer(target.Container, rmCmd))
	}()

	dumpCmd := execx.Cmd{
		Name: "redis-cli",
		Args: []string{"-p", strconv.Itoa(target.Port), "--rdb", containerRDB},
		Env:  env,
	}
	if err := deps.Run.Run(ctx, execx.InContainer(target.Container, dumpCmd)); err != nil {
		return err
	}

	copyCmd := execx.Cmd{
		Name: "docker",
		Args: []string{"cp", target.Container + ":" + containerRDB, dest},
	}
	return deps.Run.Run(ctx, copyCmd)
}

// ValidateRDB checks that the file begins with the Redis RDB magic bytes "REDIS".
func ValidateRDB(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	var header [5]byte
	n, err := io.ReadFull(file, header[:])
	if err != nil || n < 5 || string(header[:]) != "REDIS" {
		return errors.New("invalid or corrupted Redis backup: missing REDIS magic header")
	}
	return nil
}

// RedisRestoreSteps explains how to load a downloaded RDB. A running Redis cannot load an RDB
// in place, and replacing its data file is destructive, so this is never done automatically.
func RedisRestoreSteps(target *config.RedisTarget, path string) string {
	if target.Mode == config.ModeDocker {
		return fmt.Sprintf(`Downloaded to %s. To restore it into container %q:
  1. Stop Redis writes: docker stop %s
  2. Copy the file in:   docker cp %s %s:/data/dump.rdb
  3. Start it again:     docker start %s
If appendonly is enabled, move the AOF files aside first or Redis will load those instead.`,
			path, target.Container, target.Container, path, target.Container, target.Container)
	}
	return fmt.Sprintf(`Downloaded to %s. Stop Redis, copy it over its dump.rdb (see 'dir' and
'dbfilename' in redis.conf), then start Redis. If appendonly is enabled, move the AOF files aside first.`, path)
}
