package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"time"

	"backstash/internal/config"
	"backstash/internal/report"
	"backstash/internal/retention"
	"backstash/internal/storage"
	"backstash/internal/target"
)

// Verify downloads the newest backup of each target and proves it is readable, without touching
// any live database: pg_restore --list for Postgres, the RDB header for Redis, a full read of the
// archive for files. Encrypted backups are decrypted first, so this also proves the private key
// still works. It also fails a backup far smaller than its predecessors.
// shortcut: structural checks, not a trial restore into a scratch database; add one if you need it.
func (deps *Deps) Verify(ctx context.Context) report.Report {
	start := time.Now()
	rep := report.New(report.ActionVerify, deps.App.Name, deps.App.Environment)
	for _, kind := range deps.App.Kinds() {
		rep.Targets = append(rep.Targets, deps.verifyOne(ctx, kind))
	}
	rep.Duration = time.Since(start)
	return rep
}

func (deps *Deps) verifyOne(ctx context.Context, kind config.Kind) (res report.Target) {
	res.Kind = kind
	defer func() {
		if panicVal := recover(); panicVal != nil {
			res.Err = fmt.Errorf("unexpected error: %v", panicVal)
		}
	}()
	objs, err := deps.Store.List(ctx, deps.App.Prefix(kind))
	if err != nil {
		res.Err = err
		return res
	}
	var ours []storage.Object
	for _, obj := range objs {
		if retention.IsBackupName(path.Base(obj.Key)) {
			ours = append(ours, obj)
		}
	}
	latest := newest(ours, 1)
	if len(latest) == 0 {
		res.Err = errors.New("no backups found")
		return res
	}
	obj := latest[0]
	res.Name, res.Size = path.Base(obj.Key), report.ByteSize(obj.Size)

	local, err := deps.fetch(ctx, obj.Key, res.Name)
	if err != nil {
		res.Err = err
		return res
	}
	defer func() { _ = os.Remove(local) }()

	switch kind {
	case config.Postgres:
		err = deps.Target.ListPostgres(ctx, deps.App.Postgres, local)
	case config.Redis:
		err = target.ValidateRDB(local)
	default:
		err = target.VerifyFiles(local)
	}
	if err != nil {
		res.Err = fmt.Errorf("%s is not a valid backup: %w", res.Name, err)
		return res
	}
	var earlier []storage.Object
	for _, other := range ours {
		if other.Key != obj.Key {
			earlier = append(earlier, other)
		}
	}
	if warn := sizeAnomaly(earlier, obj.Size); warn != "" {
		res.Err = errors.New(warn)
	}
	return res
}
