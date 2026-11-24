package run

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"backstash/internal/report"
	"backstash/internal/storage"
)

const (
	minFreeBytes  = 256 << 20 // never start a backup with less than this free
	anomalyWindow = 5         // recent backups the size check compares against
	anomalyNeeds  = 3         // history needed before a size is judged
)

// freeBytes is the space available to us in dir.
func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // block counts are never negative
}

func (deps *Deps) free(dir string) (uint64, error) {
	if deps.FreeBytes != nil {
		return deps.FreeBytes(dir)
	}
	return freeBytes(dir)
}

// newest returns up to n of the most recent objects, newest first.
func newest(objs []storage.Object, n int) []storage.Object {
	objs = slices.Clone(objs)
	newestFirst(objs)
	return objs[:min(n, len(objs))]
}

// checkDisk fails early, with a clear message, when staging cannot hold the dump. The need is twice
// the last stored backup (the dump plus its encrypted copy), and never under minFreeBytes.
func (deps *Deps) checkDisk(history []storage.Object) error {
	dir := deps.Target.StagingDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	need := uint64(minFreeBytes)
	if last := newest(history, 1); len(last) == 1 {
		need = max(need, 2*uint64(last[0].Size)) //nolint:gosec // sizes are never negative
	}
	have, err := deps.free(dir)
	if err != nil {
		return nil // cannot tell; do not block a backup on a failed statfs
	}
	if have < need {
		return fmt.Errorf("not enough free space in %s: %s free, need about %s",
			dir, report.FormatSize(int64(have)), report.FormatSize(int64(need))) //nolint:gosec // bounded by disk size
	}
	return nil
}

// sizeAnomaly says why size looks wrong next to the recent backups ("" = fine). A dump that is
// less than half the recent median is almost always a failed export that still exited 0.
func sizeAnomaly(history []storage.Object, size int64) string {
	recent := newest(history, anomalyWindow)
	if len(recent) < anomalyNeeds {
		return ""
	}
	sizes := make([]int64, len(recent))
	for idx, obj := range recent {
		sizes[idx] = obj.Size
	}
	slices.Sort(sizes)
	median := sizes[len(sizes)/2]
	if median > 0 && size*2 < median {
		return fmt.Sprintf("backup is %s, under half the recent median of %s: check the dump is complete",
			report.FormatSize(size), report.FormatSize(median))
	}
	return ""
}

// WriteMetrics writes Prometheus textfile-collector metrics for a finished backup, atomically.
// Alert on time() - backstash_run_timestamp_seconds, or on backstash_target_success == 0.
func WriteMetrics(dir string, rep report.Report, now time.Time) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // metrics are not secret; node_exporter must read them
		return err
	}
	labels := fmt.Sprintf(`app=%q,env=%q`, rep.App, rep.Env)
	var out strings.Builder
	bool01 := func(ok bool) int {
		if ok {
			return 1
		}
		return 0
	}
	fmt.Fprintf(&out, "# TYPE backstash_run_timestamp_seconds gauge\nbackstash_run_timestamp_seconds{%s} %d\n", labels, now.Unix())
	fmt.Fprintf(&out, "# TYPE backstash_run_success gauge\nbackstash_run_success{%s} %d\n", labels, bool01(!rep.Failed()))
	fmt.Fprintf(&out, "# TYPE backstash_run_duration_seconds gauge\nbackstash_run_duration_seconds{%s} %.0f\n", labels, rep.Duration.Seconds())
	out.WriteString("# TYPE backstash_target_success gauge\n")
	for _, targetItem := range rep.Targets {
		fmt.Fprintf(&out, "backstash_target_success{%s,kind=%q} %d\n", labels, targetItem.Kind, bool01(targetItem.Err == nil))
	}
	out.WriteString("# TYPE backstash_target_size_bytes gauge\n")
	for _, targetItem := range rep.Targets {
		if targetItem.Err == nil {
			fmt.Fprintf(&out, "backstash_target_size_bytes{%s,kind=%q} %d\n", labels, targetItem.Kind, int64(targetItem.Size))
		}
	}
	path := filepath.Join(dir, fmt.Sprintf("backstash_%s_%s.prom", rep.App, rep.Env))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(out.String()), 0o644); err != nil { //nolint:gosec // see above
		return err
	}
	return os.Rename(tmp, path)
}
