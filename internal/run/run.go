// Package run orchestrates backup, restore, list and prune for one app. It is the only place
// that knows the order of steps and which failures are fatal.
package run

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"backstash/internal/config"
	"backstash/internal/encrypt"
	"backstash/internal/report"
	"backstash/internal/retention"
	"backstash/internal/storage"
	"backstash/internal/target"
)

// The operator's own choices or mistakes: shown on screen, never alerted.
var (
	ErrCancelled        = errors.New("cancelled")
	ErrInvalidSelection = errors.New("invalid selection")
	ErrNoTerminal       = errors.New("no terminal input available; pass a backup name and --yes")
)

// Deps is everything one run needs.
type Deps struct {
	Cfg    *config.Config
	App    *config.App
	Target target.Deps
	Store  storage.Store
	Log    *slog.Logger
	In     io.Reader // restore prompts
	Out    io.Writer // user-facing output (not logs)

	FreeBytes func(dir string) (uint64, error) // free space in dir; nil means ask the OS

	reader *bufio.Reader
}

func (deps *Deps) now() time.Time {
	if deps.Target.Now != nil {
		return deps.Target.Now()
	}
	return time.Now()
}

func (deps *Deps) say(format string, args ...any) { fmt.Fprintf(deps.Out, format+"\n", args...) }

func (deps *Deps) readLine() (string, error) {
	if deps.reader == nil {
		deps.reader = bufio.NewReader(deps.In)
	}
	line, err := deps.reader.ReadString('\n')
	if err != nil && line == "" {
		// EOF with no content means there is no interactive terminal available.
		if errors.Is(err, io.EOF) {
			return "", ErrNoTerminal
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// BackupOpts selects what a backup run does.
type BackupOpts struct {
	Only []config.Kind // empty means every configured target
	Keep bool          // keep the local copy after upload
}

func (deps *Deps) kinds(only []config.Kind) ([]config.Kind, error) {
	all := deps.App.Kinds()
	if len(only) == 0 {
		return all, nil
	}
	for _, kind := range only {
		if !slices.Contains(all, kind) {
			return nil, fmt.Errorf("%s is not configured for %s", kind, deps.App.Key())
		}
	}
	// keep the canonical run order, not the order typed
	var out []config.Kind
	for _, kind := range config.AllKinds {
		if slices.Contains(only, kind) {
			out = append(out, kind)
		}
	}
	return out, nil
}

func (deps *Deps) dump(ctx context.Context, kind config.Kind) (target.Artifact, error) {
	switch kind {
	case config.Postgres:
		return deps.Target.DumpPostgres(ctx, deps.App.Postgres)
	case config.Redis:
		return deps.Target.DumpRedis(ctx, deps.App.Redis)
	default:
		return deps.Target.DumpFiles(ctx, deps.App.Files)
	}
}

// backupTimeout stops a stuck dump (e.g. pg_dump waiting on a lock) so the failure alert fires.
const backupTimeout = 6 * time.Hour

// lock stops a second backup of the same app starting while one runs (the lock dies with the process).
func (deps *Deps) lock() (release func(), err error) {
	if err := os.MkdirAll(deps.Target.StagingDir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(deps.Target.StagingDir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another backup of %s is still running", deps.App.Key())
	}
	return func() { _ = file.Close() }, nil
}

// Backup dumps, uploads and prunes each target. Targets are independent: one failing does not
// skip the others, and the run fails if any did.
func (deps *Deps) Backup(ctx context.Context, options BackupOpts) report.Report {
	start := time.Now()
	rep := report.New(report.ActionBackup, deps.App.Name, deps.App.Environment)
	kinds, err := deps.kinds(options.Only)
	if err != nil {
		rep.Err = err
		return rep
	}
	release, err := deps.lock()
	if err != nil {
		rep.Err = err
		return rep
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, backupTimeout)
	defer cancel()
	deps.Log.Info("backup started", "app", deps.App.Key(), "targets", kinds)
	for _, kind := range kinds {
		rep.Targets = append(rep.Targets, deps.backupOne(ctx, kind, options))
	}
	// Leftovers from --keep or failed uploads age out with the same retention.
	// Pass per-kind prefixes so we never delete another app's local copies from
	// the shared staging directory.
	for _, kind := range kinds {
		retention.PruneLocal(deps.Target.StagingDir, deps.now(), deps.Cfg.Retention(deps.App), deps.App.TargetLabel(kind))
	}
	rep.Duration = time.Since(start)
	return rep
}

func (deps *Deps) backupOne(ctx context.Context, kind config.Kind, options BackupOpts) (res report.Target) {
	res.Kind = kind
	log := deps.Log.With("target", string(kind))
	// Last-resort boundary: a panic must still become a failed target so the alerts fire.
	defer func() {
		if panicVal := recover(); panicVal != nil {
			log.Error("unexpected panic", "panic", fmt.Sprint(panicVal))
			res.Err = fmt.Errorf("unexpected error: %v (see the run log)", panicVal)
		}
	}()

	// History is read once: it sizes the free-space check and the size sanity check.
	history, _ := deps.Store.List(ctx, deps.App.Prefix(kind))
	if err := deps.checkDisk(history); err != nil {
		log.Error("not enough disk", "err", err)
		res.Err = err
		return res
	}
	art, err := deps.dump(ctx, kind)
	if err != nil {
		log.Error("dump failed", "err", err)
		res.Err = err
		return res
	}
	log.Info("dumped", "file", art.Name, "bytes", art.Size)

	if recipient := deps.Cfg.Recipient(deps.App); recipient != "" {
		if art, err = deps.seal(art, recipient); err != nil {
			log.Error("encrypt failed", "err", err)
			res.Err = err
			return res
		}
	}
	if warn := sizeAnomaly(history, art.Size); warn != "" {
		log.Warn("backup size looks wrong", "detail", warn)
		res.Note = warn
	}

	key := deps.App.Prefix(kind) + art.Name
	if err := deps.Store.Upload(ctx, art.Path, key); err != nil {
		log.Error("upload failed", "err", err)
		res.Err = fmt.Errorf("%w (local file kept at %s)", err, art.Path)
		return res
	}
	log.Info("uploaded", "key", key)
	res.Name, res.Size = art.Name, report.ByteSize(art.Size)
	if !options.Keep {
		_ = os.Remove(art.Path)
	}

	// A prune failure must not fail a backup that was stored: it is logged and shown.
	pruned, err := retention.Prune(ctx, deps.Store, deps.App.Prefix(kind), deps.now(), deps.Cfg.Retention(deps.App),
		retention.Options{Max: deps.Cfg.MaxPrune()})
	if err != nil {
		log.Warn("prune failed", "err", err)
		res.Note = strings.TrimSpace(res.Note + " prune failed: " + err.Error())
		return res
	}
	res.Pruned = len(pruned)
	return res
}

// seal encrypts the staged dump for recipient and removes the plaintext, so only ciphertext is
// ever uploaded or kept (--keep, failed uploads).
func (deps *Deps) seal(art target.Artifact, recipient string) (target.Artifact, error) {
	sealed := art.Path + ".age"
	err := encrypt.File(art.Path, sealed, recipient)
	_ = os.Remove(art.Path)
	if err != nil {
		return art, fmt.Errorf("encrypt backup: %w", err)
	}
	info, err := os.Stat(sealed)
	if err != nil {
		return art, err
	}
	return target.Artifact{Path: sealed, Name: art.Name + ".age", Size: info.Size()}, nil
}

// fetch downloads one backup into staging and decrypts it if it is an .age file. It returns the
// path of the plaintext; the caller removes it.
func (deps *Deps) fetch(ctx context.Context, key, base string) (string, error) {
	if err := os.MkdirAll(deps.Target.StagingDir, 0o700); err != nil {
		return "", err
	}
	local := filepath.Join(deps.Target.StagingDir, base)
	if err := deps.Store.Download(ctx, key, local); err != nil {
		return "", err
	}
	plain, encrypted := strings.CutSuffix(local, ".age")
	if !encrypted {
		return local, nil
	}
	defer func() { _ = os.Remove(local) }()
	identity, ok := deps.Target.Secrets.Get(SecretAgeIdentity)
	if !ok {
		return "", fmt.Errorf("%s is encrypted and secret %s is not set", base, SecretAgeIdentity)
	}
	if err := encrypt.Decrypt(local, plain, identity); err != nil {
		return "", err
	}
	return plain, nil
}

// PruneOpts selects how a prune behaves.
type PruneOpts struct {
	DryRun bool // report what would be deleted, delete nothing
	Force  bool // lift the max_prune safety limit
}

// Prune applies retention to every target of the app without taking a backup.
func (deps *Deps) Prune(ctx context.Context, opts PruneOpts) report.Report {
	rep := report.New(report.ActionPrune, deps.App.Name, deps.App.Environment)
	limit := deps.Cfg.MaxPrune()
	if opts.Force {
		limit = 0
	}
	for _, kind := range deps.App.Kinds() {
		targetResult := report.Target{Kind: kind}
		gone, err := retention.Prune(ctx, deps.Store, deps.App.Prefix(kind), deps.now(), deps.Cfg.Retention(deps.App),
			retention.Options{DryRun: opts.DryRun, Max: limit})
		targetResult.Err = err
		if opts.DryRun {
			targetResult.Note = fmt.Sprintf("dry run: would remove %d backup(s)", len(gone))
			for _, obj := range gone {
				deps.say("would delete %s", obj.Key)
			}
		} else {
			targetResult.Pruned = len(gone)
		}
		rep.Targets = append(rep.Targets, targetResult)
	}
	return rep
}

// describe is one backup as shown by list and the restore picker.
func describe(obj storage.Object) string {
	return fmt.Sprintf("%s  %s  %s", path.Base(obj.Key), report.FormatSize(obj.Size), obj.Modified.UTC().Format("2006-01-02 15:04"))
}

func newestFirst(objs []storage.Object) {
	sort.Slice(objs, func(firstIdx, secondIdx int) bool { return objs[firstIdx].Modified.After(objs[secondIdx].Modified) })
}

// List prints the stored backups of each target, newest first.
func (deps *Deps) List(ctx context.Context, only []config.Kind) error {
	kinds, err := deps.kinds(only)
	if err != nil {
		return err
	}
	for _, kind := range kinds {
		objs, err := deps.Store.List(ctx, deps.App.Prefix(kind))
		if err != nil {
			return err
		}
		newestFirst(objs)
		deps.say("%s (%s)", kind, deps.App.Prefix(kind))
		if len(objs) == 0 {
			deps.say("  (none)")
		}
		for _, obj := range objs {
			deps.say("  %s", describe(obj))
		}
	}
	return nil
}

func (deps *Deps) pick(ctx context.Context, kind config.Kind) (string, error) {
	objs, err := deps.Store.List(ctx, deps.App.Prefix(kind))
	if err != nil {
		return "", err
	}
	if len(objs) == 0 {
		return "", errors.New("no backups found")
	}
	newestFirst(objs)
	for idx, obj := range objs {
		deps.say("%3d. %s", idx+1, describe(obj))
	}
	fmt.Fprint(deps.Out, "Restore which backup? [number, blank to cancel]: ")
	choice, err := deps.readLine()
	if err != nil {
		return "", err
	}
	if choice == "" {
		return "", ErrCancelled
	}
	selectionIndex, err := strconv.Atoi(choice)
	if err != nil || selectionIndex < 1 || selectionIndex > len(objs) {
		return "", ErrInvalidSelection
	}
	return objs[selectionIndex-1].Key, nil
}

// confirmTarget is what the operator must type to allow a destructive restore ("" = nothing is
// overwritten, so no confirmation).
func (deps *Deps) confirmTarget(kind config.Kind) string {
	switch kind {
	case config.Postgres:
		return deps.App.Postgres.Database
	case config.Files:
		if deps.App.Files.Mode == config.ModeDocker {
			return deps.App.Files.Container
		}
		return deps.App.Files.Path
	}
	return ""
}

// confirm asks the operator to type the target name unless yes. Cancelling is an error value.
func (deps *Deps) confirm(kind config.Kind, backup string, yes bool) error {
	want := deps.confirmTarget(kind)
	if want == "" || yes {
		return nil
	}
	deps.say("This will OVERWRITE '%s' with %s.", want, backup)
	fmt.Fprintf(deps.Out, "Type '%s' to continue: ", want)
	typed, err := deps.readLine()
	if err != nil {
		return err
	}
	if typed != want {
		return ErrCancelled
	}
	return nil
}

// resolveKey is the object to restore: the one named, or the one picked from the list.
func (deps *Deps) resolveKey(ctx context.Context, kind config.Kind, name string) (string, error) {
	prefix := deps.App.Prefix(kind)
	switch {
	case name == "":
		return deps.pick(ctx, kind)
	case strings.HasPrefix(name, prefix):
		return name, nil
	default:
		return prefix + name, nil
	}
}

// silent marks the operator's own choices or mistakes: shown on screen, never alerted.
func silent(rep report.Report, err error) report.Report {
	rep.Err = err
	rep.Silent = errors.Is(err, ErrCancelled) || errors.Is(err, ErrInvalidSelection) || errors.Is(err, ErrNoTerminal)
	return rep
}

// Restore fetches a backup (name, or chosen from a list) and loads it. Postgres and files are
// overwritten after the operator types the target name (unless yes). Redis is downloaded and
// the manual steps printed, because a running Redis cannot load an RDB in place.
func (deps *Deps) Restore(ctx context.Context, kind config.Kind, name string, yes bool) report.Report {
	start := time.Now()
	rep := report.New(report.ActionRestore, deps.App.Name, deps.App.Environment)
	if targetResult, err := deps.restore(ctx, kind, name, yes); err != nil {
		rep = silent(rep, err)
	} else {
		rep.Targets = []report.Target{targetResult}
	}
	rep.Duration = time.Since(start)
	return rep
}

// restore does the work. A returned error means nothing was attempted (bad choice, cancelled,
// misconfigured); a failure of the restore itself is reported in the returned Target.
func (deps *Deps) restore(ctx context.Context, kind config.Kind, name string, yes bool) (report.Target, error) {
	if !slices.Contains(deps.App.Kinds(), kind) {
		return report.Target{}, fmt.Errorf("%s is not configured for %s", kind, deps.App.Key())
	}
	key, err := deps.resolveKey(ctx, kind, name)
	if err != nil {
		return report.Target{}, err
	}
	// Basename only, so the key can never escape the staging folder.
	base := path.Base(key)
	if base == "." || base == ".." || base == "/" || strings.Contains(base, `\`) {
		return report.Target{}, ErrInvalidSelection
	}
	if err := deps.confirm(kind, base, yes); err != nil {
		return report.Target{}, err
	}
	targetResult := report.Target{Kind: kind, Name: base}
	local, err := deps.fetch(ctx, key, base)
	if err != nil {
		targetResult.Err = err
		return targetResult, nil
	}
	if info, err := os.Stat(local); err == nil {
		targetResult.Size = report.ByteSize(info.Size())
	}

	switch kind {
	case config.Redis:
		if err := target.ValidateRDB(local); err != nil {
			targetResult.Err = err
			return targetResult, nil
		}
		deps.say("%s", target.RedisRestoreSteps(deps.App.Redis, local))
		return targetResult, nil // keep the file: the operator needs it
	case config.Postgres:
		err = deps.Target.RestorePostgres(ctx, deps.App.Postgres, local)
	default:
		err = deps.Target.RestoreFiles(ctx, deps.App.Files, local)
	}
	if err != nil {
		// Keep the download so the restore can be retried without downloading again.
		targetResult.Err = fmt.Errorf("%w (backup kept at %s)", err, local)
		return targetResult, nil
	}
	_ = os.Remove(local)
	return targetResult, nil
}
