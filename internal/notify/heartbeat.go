package notify

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Heartbeat pings a dead-man's-switch URL (healthchecks.io, Better Stack, ...). Success hits the
// URL itself; failure hits <url>/fail. If the pings stop arriving the monitor alerts, which covers
// what Slack and email cannot: cron not firing, the host down, the binary gone.
func Heartbeat(ctx context.Context, url string, failed bool) error {
	if failed {
		url = strings.TrimRight(url, "/") + "/fail"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var err error
	for try := 0; try < 3; try++ {
		var req *http.Request
		if req, err = http.NewRequestWithContext(ctx, http.MethodGet, url, nil); err != nil {
			return err
		}
		var res *http.Response
		if res, err = http.DefaultClient.Do(req); err == nil {
			_ = res.Body.Close()
			if res.StatusCode < 300 {
				return nil
			}
			err = fmt.Errorf("heartbeat: HTTP %d", res.StatusCode)
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(time.Duration(try+1) * 500 * time.Millisecond)
	}
	return err
}
