package runlog

import (
	"bytes"
	"strings"
	"testing"
)

func TestCaptureTeesAndKeepsDerivedLoggers(testState *testing.T) {
	var out bytes.Buffer
	capture := New(&out)
	log := capture.Logger()
	log.Info("starting", "app", "myapp")
	log.With("target", "redis").Error("boom")

	got := capture.Text()
	for _, want := range []string{"starting", "app=myapp", "boom", "target=redis"} {
		if !strings.Contains(got, want) {
			testState.Errorf("capture missing %q:\n%s", want, got)
		}
	}
	if out.String() != got {
		testState.Errorf("stderr copy differs from capture")
	}
}
