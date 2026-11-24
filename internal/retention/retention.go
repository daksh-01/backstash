// Package retention deletes backups older than the retention window.
package retention

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"backstash/internal/storage"
)

// nameRe matches any backup file we created: <anything>-YYYYmmdd-HHMMSS.{dump,rdb,tar.gz}.
// The leading .+ intentionally matches any label; callers filter by prefix separately.
var nameRe = regexp.MustCompile(`^.+-\d{8}-\d{6}(?:\.dump|\.rdb|\.tar\.gz)$`)

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

// Prune deletes expired backups under prefix and returns how many it removed.
func Prune(ctx context.Context, store storage.Store, prefix string, now time.Time, days int) (int, error) {
	if days <= 0 {
		return 0, nil
	}
	objs, err := store.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	expired := Expired(objs, now, days)
	if len(expired) == 0 {
		return 0, nil
	}
	keys := make([]string, len(expired))
	for idx, obj := range expired {
		keys[idx] = obj.Key
	}
	if err := store.Delete(ctx, keys); err != nil {
		return 0, err
	}
	return len(keys), nil
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
