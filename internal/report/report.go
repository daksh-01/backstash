// Package report is what one finished run looks like, shared by Slack and email.
package report

import (
	"fmt"
	"os"
	"time"

	"backstash/internal/config"
)

// Action represents the kind of operation performed.
type Action string

const (
	ActionBackup  Action = "Backup"
	ActionRestore Action = "Restore"
	ActionPrune   Action = "Prune"
	ActionVerify  Action = "Verify"
)

// ByteSize represents data size in bytes.
type ByteSize int64

// String formats bytes as a decimal megabyte string with one decimal place.
func (b ByteSize) String() string {
	return fmt.Sprintf("%.1f MB", float64(b)/1_000_000)
}

// Target is the outcome for one kind (postgres, redis, files) in a run.
type Target struct {
	Kind   config.Kind
	Name   string // backup file name; set on success
	Size   ByteSize
	Pruned int    // old backups removed by retention
	Note   string // non-fatal problem, e.g. a failed prune
	Err    error  // set on failure
}

// Report is one backup or restore run.
type Report struct {
	Action   Action
	App, Env string
	Host     string
	Duration time.Duration
	Targets  []Target
	// Silent runs (cancelled, invalid choice, no terminal) print only and send no alerts.
	Silent bool
	// Err is a failure that is not tied to one target (e.g. cancelled).
	Err error
}

// New starts a report for this host.
func New(action Action, app, env string) Report {
	host, _ := os.Hostname()
	return Report{Action: action, App: app, Env: env, Host: host}
}

// Failed is true if the run or any target failed.
func (rep Report) Failed() bool {
	if rep.Err != nil {
		return true
	}
	for _, target := range rep.Targets {
		if target.Err != nil {
			return true
		}
	}
	return false
}

// FailedKinds lists the kinds that failed, in run order.
func (rep Report) FailedKinds() []config.Kind {
	var out []config.Kind
	for _, target := range rep.Targets {
		if target.Err != nil {
			out = append(out, target.Kind)
		}
	}
	return out
}

// TotalSize sums the sizes of the successful targets.
func (rep Report) TotalSize() ByteSize {
	var total ByteSize
	for _, target := range rep.Targets {
		if target.Err == nil {
			total += target.Size
		}
	}
	return total
}

// TotalPruned sums the old backups removed.
func (rep Report) TotalPruned() int {
	total := 0
	for _, target := range rep.Targets {
		total += target.Pruned
	}
	return total
}

// FormatSize formats byte size into decimal megabytes (e.g. "84.2 MB").
func FormatSize(bytes int64) string { return ByteSize(bytes).String() }

// FormatDuration formats duration into human-readable minutes and seconds.
func FormatDuration(duration time.Duration) string {
	totalSeconds := int(duration.Round(time.Second).Seconds())
	if totalSeconds >= 60 {
		return fmt.Sprintf("%dm %ds", totalSeconds/60, totalSeconds%60)
	}
	return fmt.Sprintf("%ds", totalSeconds)
}
