package retention

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"backstash/internal/storage"
)

var now = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func obj(key string, daysAgo int) storage.Object {
	return storage.Object{Key: key, Modified: now.AddDate(0, 0, -daysAgo)}
}

func keys(objects []storage.Object) []string {
	var out []string
	for _, objItem := range objects {
		out = append(out, objItem.Key)
	}
	return out
}

func TestExpiredDeletesOnlyOldOnesNeverTheNewest(testState *testing.T) {
	objs := []storage.Object{
		obj("a/prod/postgres/db-20261001-020000.dump", 53),
		obj("a/prod/postgres/db-20261101-020000.dump", 22),
		obj("a/prod/postgres/db-20261120-020000.dump", 3),
	}
	got := keys(Expired(objs, now, 30))
	if len(got) != 1 || got[0] != "a/prod/postgres/db-20261001-020000.dump" {
		testState.Errorf("got %v", got)
	}
}

func TestNewestIsKeptEvenWhenEverythingIsOld(testState *testing.T) {
	objs := []storage.Object{
		obj("p/db-20260801-020000.dump", 114),
		obj("p/db-20260901-020000.dump", 83),
	}
	got := keys(Expired(objs, now, 30))
	if len(got) != 1 || got[0] != "p/db-20260801-020000.dump" {
		testState.Errorf("the newest must survive a long outage, got %v", got)
	}
}

func TestExpiredLeavesForeignFilesAlone(testState *testing.T) {
	objs := []storage.Object{
		obj("p/notes.txt", 400),
		obj("p/db-20260101-020000.dump.bak", 400),
		obj("p/db-20260101-020000.dump", 400),
		obj("p/db-20261120-020000.dump", 3),
	}
	got := keys(Expired(objs, now, 30))
	if len(got) != 1 || got[0] != "p/db-20260101-020000.dump" {
		testState.Errorf("got %v", got)
	}
}

func TestStrayNewerFileDoesNotProtectOldBackups(testState *testing.T) {
	objs := []storage.Object{
		obj("p/readme.txt", 0), // newest object, but not ours
		obj("p/db-20260101-020000.dump", 300),
		obj("p/db-20260201-020000.dump", 290),
	}
	got := keys(Expired(objs, now, 30))
	if len(got) != 1 || got[0] != "p/db-20260101-020000.dump" {
		testState.Errorf("newest *backup* must be kept, got %v", got)
	}
}

func TestEveryKindOfBackupNameMatches(testState *testing.T) {
	// The orchestrator lists one prefix (one kind) at a time, so test each kind on its own.
	for _, testCase := range [][2]string{
		{"p/redis-20260101-020000.rdb", "p/redis-20260102-020000.rdb"},
		{"p/up-20260101-020000.tar.gz", "p/up-20260102-020000.tar.gz"},
		{"p/db-20260101-020000.dump", "p/db-20260102-020000.dump"},
	} {
		got := keys(Expired([]storage.Object{obj(testCase[0], 300), obj(testCase[1], 299)}, now, 30))
		if len(got) != 1 || got[0] != testCase[0] {
			testState.Errorf("%v: got %v", testCase, got)
		}
	}
}

func TestZeroDaysDisablesPruning(testState *testing.T) {
	objs := []storage.Object{obj("p/db-20200101-020000.dump", 2000), obj("p/db-20200102-020000.dump", 1999)}
	if got := Expired(objs, now, 0); len(got) != 0 {
		testState.Errorf("got %v", keys(got))
	}
	if got := Expired(nil, now, 30); len(got) != 0 {
		testState.Errorf("got %v", keys(got))
	}
}

type memStore struct {
	storage.Store
	objs    []storage.Object
	deleted []string
}

func (memoryStore *memStore) List(context.Context, string) ([]storage.Object, error) {
	return memoryStore.objs, nil
}
func (memoryStore *memStore) Delete(_ context.Context, keys []string) error {
	memoryStore.deleted = keys
	return nil
}

func TestPruneDeletesAndCounts(testState *testing.T) {
	memoryStore := &memStore{objs: []storage.Object{
		obj("p/db-20260101-020000.dump", 300), obj("p/db-20261120-020000.dump", 3),
	}}
	pruned, err := Prune(context.Background(), memoryStore, "p/", now, 30, Options{})
	if err != nil || len(pruned) != 1 || memoryStore.deleted[0] != "p/db-20260101-020000.dump" {
		testState.Errorf("n=%d err=%v deleted=%v", len(pruned), err, memoryStore.deleted)
	}
}

func manyOld() *memStore {
	store := &memStore{objs: []storage.Object{obj("p/db-20261120-020000.dump", 3)}}
	for day := 100; day < 105; day++ {
		store.objs = append(store.objs, obj("p/db-2026010"+string(rune('0'+day-100))+"-020000.dump", day))
	}
	return store
}

func TestPruneDryRunDeletesNothingButReports(testState *testing.T) {
	store := manyOld()
	pruned, err := Prune(context.Background(), store, "p/", now, 30, Options{DryRun: true})
	if err != nil || len(pruned) != 5 || store.deleted != nil {
		testState.Fatalf("pruned=%d err=%v deleted=%v", len(pruned), err, store.deleted)
	}
}

func TestPruneRefusesToDeleteMoreThanTheLimit(testState *testing.T) {
	store := manyOld()
	_, err := Prune(context.Background(), store, "p/", now, 30, Options{Max: 3})
	var limit LimitError
	if !errors.As(err, &limit) || limit.Count != 5 || store.deleted != nil {
		testState.Fatalf("err=%v deleted=%v", err, store.deleted)
	}
	if pruned, err := Prune(context.Background(), store, "p/", now, 30, Options{Max: 5}); err != nil || len(pruned) != 5 {
		testState.Fatalf("at the limit must pass: %d %v", len(pruned), err)
	}
}

func TestEncryptedBackupsAreRetainedLikeOthers(testState *testing.T) {
	if !IsBackupName("db-20261120-020000.dump.age") || IsBackupName("db-20261120-020000.dump.age.tmp") {
		testState.Fatal("nameRe must accept exactly the .age suffix")
	}
}

func TestPruneLocalPrefix(testState *testing.T) {
	dir := testState.TempDir()
	mine := filepath.Join(dir, "shop-20260101-020000.dump")
	theirs := filepath.Join(dir, "shop_old-20260101-020000.dump")
	for _, filePath := range []string{mine, theirs} {
		_ = os.WriteFile(filePath, []byte("x"), 0o600)
		_ = os.Chtimes(filePath, now.AddDate(0, 0, -300), now.AddDate(0, 0, -300))
	}
	if prunedCount := PruneLocal(dir, now, 30, "shop"); prunedCount != 1 {
		testState.Errorf("removed %d", prunedCount)
	}
	if _, err := os.Stat(theirs); err != nil {
		testState.Error("another label's file was deleted")
	}
}

func TestPruneLocal(testState *testing.T) {
	dir := testState.TempDir()
	old := filepath.Join(dir, "db-20260101-020000.dump")
	fresh := filepath.Join(dir, "db-20261120-020000.dump")
	other := filepath.Join(dir, "notes.txt")
	for _, filePath := range []string{old, fresh, other} {
		_ = os.WriteFile(filePath, []byte("x"), 0o600)
	}
	_ = os.Chtimes(old, now.AddDate(0, 0, -300), now.AddDate(0, 0, -300))
	_ = os.Chtimes(other, now.AddDate(0, 0, -300), now.AddDate(0, 0, -300))
	if prunedCount := PruneLocal(dir, now, 30, ""); prunedCount != 1 {
		testState.Errorf("removed %d", prunedCount)
	}
	for _, filePath := range []string{fresh, other} {
		if _, err := os.Stat(filePath); err != nil {
			testState.Errorf("%s should remain", filePath)
		}
	}
}
