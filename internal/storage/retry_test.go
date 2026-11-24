package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

type flakyStore struct {
	Store
	failures, calls int
}

func (store *flakyStore) Upload(context.Context, string, string) error {
	store.calls++
	if store.calls <= store.failures {
		return errors.New("reset")
	}
	return nil
}

func TestRetryingUploadRecoversThenGivesUp(testState *testing.T) {
	var slept []time.Duration
	sleep := func(dur time.Duration) { slept = append(slept, dur) }

	store := &flakyStore{failures: 2}
	if err := (Retrying{Store: store, Sleep: sleep}).Upload(context.Background(), "p", "k"); err != nil || store.calls != 3 {
		testState.Fatalf("calls=%d err=%v", store.calls, err)
	}
	if len(slept) != 2 || slept[1] <= slept[0] {
		testState.Fatalf("backoff %v", slept)
	}

	store = &flakyStore{failures: 99}
	if err := (Retrying{Store: store, Sleep: sleep}).Upload(context.Background(), "p", "k"); err == nil || store.calls != 3 {
		testState.Fatalf("calls=%d err=%v", store.calls, err)
	}
}
