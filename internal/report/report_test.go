package report

import (
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"backstash/internal/config"
)

func TestNewReport(testState *testing.T) {
	rep := New(ActionBackup, "myapp", "prod")
	if rep.Action != ActionBackup || rep.App != "myapp" || rep.Env != "prod" {
		testState.Fatalf("unexpected report fields: %+v", rep)
	}
	host, _ := os.Hostname()
	if rep.Host != host {
		testState.Errorf("host = %q, want %q", rep.Host, host)
	}
}

func TestReportFailedAndFailedKinds(testState *testing.T) {
	rep := Report{
		Targets: []Target{
			{Kind: config.Postgres, Name: "a.dump", Size: 1000},
			{Kind: config.Redis, Err: errors.New("timeout")},
			{Kind: config.Files, Name: "c.tar.gz", Size: 2000},
		},
	}
	if !rep.Failed() {
		testState.Error("expected rep.Failed() to be true")
	}
	failed := rep.FailedKinds()
	if !slices.Equal(failed, []config.Kind{config.Redis}) {
		testState.Errorf("failed kinds = %v, want [redis]", failed)
	}

	repErr := Report{Err: errors.New("fatal")}
	if !repErr.Failed() {
		testState.Error("expected repErr.Failed() to be true")
	}

	okRep := Report{
		Targets: []Target{
			{Kind: config.Postgres, Name: "a.dump", Size: 1000},
		},
	}
	if okRep.Failed() {
		testState.Error("expected okRep.Failed() to be false")
	}
	if len(okRep.FailedKinds()) != 0 {
		testState.Errorf("expected no failed kinds, got %v", okRep.FailedKinds())
	}
}

func TestTotalSizeAndPruned(testState *testing.T) {
	rep := Report{
		Targets: []Target{
			{Kind: config.Postgres, Name: "a.dump", Size: 5_000_000, Pruned: 3},
			{Kind: config.Redis, Err: errors.New("fail"), Size: 99_000_000, Pruned: 1},
			{Kind: config.Files, Name: "b.tar.gz", Size: 15_000_000, Pruned: 2},
		},
	}
	if total := rep.TotalSize(); total != 20_000_000 {
		testState.Errorf("total size = %d, want 20000000", total)
	}
	if totalPruned := rep.TotalPruned(); totalPruned != 6 {
		testState.Errorf("total pruned = %d, want 6", totalPruned)
	}
}

func TestByteSizeStringAndFormatSize(testState *testing.T) {
	testCases := []struct {
		bytes int64
		want  string
	}{
		{0, "0.0 MB"},
		{1_000_000, "1.0 MB"},
		{85_200_000, "85.2 MB"},
		{500_000, "0.5 MB"},
	}
	for _, tc := range testCases {
		if got := ByteSize(tc.bytes).String(); got != tc.want {
			testState.Errorf("ByteSize(%d).String() = %q, want %q", tc.bytes, got, tc.want)
		}
		if got := FormatSize(tc.bytes); got != tc.want {
			testState.Errorf("FormatSize(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

func TestFormatDuration(testState *testing.T) {
	testCases := []struct {
		dur  time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m 0s"},
		{75 * time.Second, "1m 15s"},
		{125 * time.Second, "2m 5s"},
	}
	for _, tc := range testCases {
		if got := FormatDuration(tc.dur); got != tc.want {
			testState.Errorf("FormatDuration(%v) = %q, want %q", tc.dur, got, tc.want)
		}
	}
}
