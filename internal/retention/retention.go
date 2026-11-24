// Package retention deletes backups older than the retention window.
package retention

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"backstash/internal/storage"
)

// nameRe matches any backup file we created: <anything>-YYYYmmdd-HHMMSS.{dump,rdb,tar.gz}[.age].
// The leading .+ intentionally matches any label; callers filter by prefix separately.
var nameRe = regexp.MustCompile(`^.+-\d{8}-\d{6}(?:\.dump|\.rdb|\.tar\.gz)(?:\.age)?$`)

// IsBackupName reports whether name looks like a backup this tool wrote.
func IsBackupName(name string) bool { return nameRe.MatchString(name) }

// Expired returns the backups older than days, never including the newest one: a wrong clock
// or a long outage can then never leave you with no backup at all. days <= 0 disables pruning.
// Pass the objects of ONE kind (one prefix); the caller lists per kind.
func Expired(objs []storage.Object, now time.Time, days int) []storage.Object {
	if days <= 0 {
		return nil
	}
	var ours []storage.Object
	var newest *storage.Object
	for idx := range objs {
		if !nameRe.MatchString(path.Base(objs[idx].Key)) {
			continue
		}
		ours = append(ours, objs[idx])
		if newest == nil || objs[idx].Modified.After(newest.Modified) {
			newest = &objs[idx]
		}
	}
	cutoff := now.AddDate(0, 0, -days)
	var expired []storage.Object
	for _, obj := range ours {
		if obj.Modified.Before(cutoff) && obj.Key != newest.Key {
			expired = append(expired, obj)
		}
	}
	return expired
}

// Options make a prune safer. DryRun deletes nothing. Max > 0 refuses to delete more than that
// many backups in one run, so a typo in retention_days or a wrong clock cannot wipe the history.
type Options struct {
	DryRun bool
	Max    int
}

// LimitError says a prune was refused for being too large.
type LimitError struct{ Count, Max int }

func (err LimitError) Error() string {
	return fmt.Sprintf("retention would delete %d backups, over the limit of %d; check retention_days and the clock, then run `backstash prune <app> --force` if that is intended", err.Count, err.Max)
}

// Prune deletes expired backups under prefix and returns them (in a dry run: the ones it would delete).
func Prune(ctx context.Context, store storage.Store, prefix string, now time.Time, days int, opts Options) ([]storage.Object, error) {
	if days <= 0 {
		return nil, nil
	}
	objs, err := store.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	expired := Expired(objs, now, days)
	if len(expired) == 0 {
		return nil, nil
	}
	if opts.Max > 0 && len(expired) > opts.Max {
		return nil, LimitError{len(expired), opts.Max}
	}
	if opts.DryRun {
		return expired, nil
	}
	keys := make([]string, len(expired))
	for idx, obj := range expired {
		keys[idx] = obj.Key
	}
	if err := store.Delete(ctx, keys); err != nil {
		return nil, err
	}
	return expired, nil
}

// PruneLocal deletes expired kept backups from the staging folder (`--keep`, or failed uploads).
// prefix filters to files whose name starts with that string (e.g. the database or label name),
// so that one app's short retention window never deletes another app's kept backups from the
// shared staging directory.
func PruneLocal(dir string, now time.Time, days int, prefix string) int {
	if days <= 0 {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cutoff := now.AddDate(0, 0, -days)
	removed := 0
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || entry.IsDir() || !nameRe.MatchString(entry.Name()) || !info.ModTime().Before(cutoff) {
			continue
		}
		if prefix != "" && !strings.HasPrefix(entry.Name(), prefix+"-") {
			continue
		}
		if os.Remove(filepath.Join(dir, entry.Name())) == nil {
			removed++
		}
	}
	return removed
}
