package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestHeartbeatPathsAndRetry(testState *testing.T) {
	var mu sync.Mutex
	var paths []string
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.URL.Path)
		if hits++; hits == 1 {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	if err := Heartbeat(context.Background(), srv.URL+"/uuid/", false); err != nil {
		testState.Fatal(err)
	}
	if err := Heartbeat(context.Background(), srv.URL+"/uuid/", true); err != nil {
		testState.Fatal(err)
	}
	if len(paths) != 3 || paths[0] != "/uuid/" || paths[2] != "/uuid/fail" {
		testState.Fatalf("paths %v", paths)
	}
}
