package storage

import (
	"context"
	"time"
)

// Retrying retries a failed Upload from the start. The SDK already retries single requests; this
// covers a whole transfer dying on a network blip, which is what usually fails a nightly upload.
// shortcut: no multipart resume, a retry re-sends the file; upgrade if backups get very large.
type Retrying struct {
	Store
	Attempts int                 // total tries, default 3
	Sleep    func(time.Duration) // default time.Sleep
}

func (retry Retrying) Upload(ctx context.Context, path, key string) error {
	attempts, sleep := retry.Attempts, retry.Sleep
	if attempts < 1 {
		attempts = 3
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	var err error
	for try := 1; try <= attempts; try++ {
		if err = retry.Store.Upload(ctx, path, key); err == nil || ctx.Err() != nil {
			return err
		}
		if try < attempts {
			sleep(time.Duration(try) * 10 * time.Second)
		}
	}
	return err
}
