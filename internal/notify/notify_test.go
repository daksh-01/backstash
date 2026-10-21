package notify

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"backstash/internal/config"
	"backstash/internal/report"
)

func okReport() report.Report {
	return report.Report{Action: "Backup", App: "myapp", Env: "prod", Host: "server-1", Duration: 75 * time.Second,
		Targets: []report.Target{
			{Kind: config.Postgres, Name: "myapp-20261123-020005.dump", Size: 84_200_000, Pruned: 2},
			{Kind: config.Redis, Name: "redis-20261123-020030.rdb", Size: 1_000_000, Note: "prune failed: timeout"},
		}}
}

func failedReport() report.Report {
	rep := okReport()
	rep.Targets[1] = report.Target{Kind: config.Redis, Err: errors.New("redis-cli failed (exit 1): NOAUTH <secret> & co")}
	return rep
}

func TestSlackCardSuccess(testState *testing.T) {
	msg := BuildSlack(okReport(), time.Date(2026, 11, 23, 2, 1, 20, 0, time.UTC))
	raw, _ := json.Marshal(msg)
	slackJSON := string(raw)
	for _, want := range []string{"Backup Successful", "85.2 MB", "1m 15s", "Old backups removed", "myapp-20261123-020005.dump", "prune failed", successColor} {
		if !strings.Contains(slackJSON, want) {
			testState.Errorf("card missing %q:\n%s", want, slackJSON)
		}
	}
}

func TestSlackCardFailureEscapesAndNamesTheTarget(testState *testing.T) {
	raw, _ := json.Marshal(BuildSlack(failedReport(), time.Now()))
	slackJSON := string(raw)
	if !strings.Contains(slackJSON, "Backup Failed") || !strings.Contains(slackJSON, failureColor) || !strings.Contains(slackJSON, "NOAUTH") {
		testState.Errorf("bad failure card:\n%s", slackJSON)
	}
	if strings.Contains(slackJSON, "<secret>") || !strings.Contains(slackJSON, `\u0026lt;secret\u0026gt;`) {
		testState.Errorf("markup characters not escaped:\n%s", slackJSON)
	}
}

func TestSlackPostsWithTokenAndChannel(testState *testing.T) {
	var gotAuth, gotChannel string
	srv := httptest.NewServer(http.HandlerFunc(func(respWriter http.ResponseWriter, req *http.Request) {
		gotAuth = req.Header.Get("Authorization")
		var in map[string]any
		_ = json.NewDecoder(req.Body).Decode(&in)
		gotChannel, _ = in["channel"].(string)
		_, _ = io.WriteString(respWriter, `{"ok":true}`)
	}))
	defer srv.Close()
	slackClient := &Slack{Token: "xoxb-1", Channel: "C123", URL: srv.URL}
	if err := slackClient.Notify(context.Background(), okReport()); err != nil {
		testState.Fatal(err)
	}
	if gotAuth != "Bearer xoxb-1" || gotChannel != "C123" {
		testState.Errorf("auth=%q channel=%q", gotAuth, gotChannel)
	}
}

func TestSlackErrorShowsCodeNotToken(testState *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(respWriter http.ResponseWriter, req *http.Request) {
		_, _ = io.WriteString(respWriter, `{"ok":false,"error":"channel_not_found"}`)
	}))
	defer srv.Close()
	err := (&Slack{Token: "xoxb-secret", Channel: "C", URL: srv.URL}).Notify(context.Background(), okReport())
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") || strings.Contains(err.Error(), "xoxb-secret") {
		testState.Fatalf("got %v", err)
	}
}

func TestSlackRetriesRateLimits(testState *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(respWriter http.ResponseWriter, req *http.Request) {
		if calls.Add(1) < 3 {
			respWriter.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(respWriter, `{"ok":true}`)
	}))
	defer srv.Close()
	slackClient := &Slack{Token: "tokenVal", Channel: "channelVal", URL: srv.URL, Retries: 2, Sleep: func(time.Duration) {}}
	if err := slackClient.Notify(context.Background(), okReport()); err != nil || calls.Load() != 3 {
		testState.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}

func TestSlackRetriesWithRetryAfter(testState *testing.T) {
	var calls atomic.Int32
	var sleptDuration time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(respWriter http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 1 {
			respWriter.Header().Set("Retry-After", "5")
			respWriter.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(respWriter, `{"ok":true}`)
	}))
	defer srv.Close()
	slackClient := &Slack{
		Token:   "tokenVal",
		Channel: "channelVal",
		URL:     srv.URL,
		Retries: 2,
		Sleep: func(d time.Duration) {
			sleptDuration = d
		},
	}
	if err := slackClient.Notify(context.Background(), okReport()); err != nil || calls.Load() != 2 {
		testState.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
	if sleptDuration != 5*time.Second {
		testState.Errorf("slept = %v, want 5s", sleptDuration)
	}
}

func TestMailEmptyRecipientsFails(testState *testing.T) {
	mailer := &Mail{Host: "127.0.0.1", Port: 587, To: " ,  ,"}
	if err := mailer.SendFailure(context.Background(), failedReport(), ""); err == nil || !strings.Contains(err.Error(), "no valid recipient") {
		testState.Fatalf("expected error for empty recipients, got %v", err)
	}
}

func TestMailSubjectAndAttachment(testState *testing.T) {
	mailer := &Mail{From: "bot@x.com", To: "alerts@example.com"}
	raw := string(mailer.BuildMail(failedReport(), "line1\nline2 boom\n", time.Now()))
	for _, want := range []string{"To: alerts@example.com", "Subject: [backstash] backup FAILED: myapp/prod (redis)", `filename="run.log"`, "NOAUTH"} {
		if !strings.Contains(raw, want) {
			testState.Errorf("mail missing %q:\n%s", want, raw)
		}
	}
	enc := base64.StdEncoding.EncodeToString([]byte("line1\nline2 boom\n"))
	if !strings.Contains(strings.ReplaceAll(raw, "\r\n", ""), enc) {
		testState.Error("run log not attached")
	}
}

func TestMailHeaderInjectionBlocked(testState *testing.T) {
	rep := failedReport()
	rep.App = "evil\r\nBcc: x@y.z"
	raw := string(((&Mail{From: "a@b.c", To: "d@e.f"}).BuildMail(rep, "", time.Now())))
	headers, _, _ := strings.Cut(raw, "\r\n\r\n")
	if strings.Contains(headers, "\r\nBcc:") || strings.Contains(headers, "\nBcc:") {
		testState.Errorf("header injected:\n%s", headers)
	}
}

// smtpStub accepts one plain SMTP message and returns the DATA it received.
func smtpStub(testState *testing.T) (addr string, got chan string) {
	testState.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		testState.Fatal(err)
	}
	testState.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := textproto.NewReader(bufio.NewReader(conn))
		say := func(msg string) { fmt.Fprintf(conn, "%s\r\n", msg) }
		say("220 stub")
		for {
			line, err := reader.ReadLine()
			if err != nil {
				return
			}
			switch cmd := strings.ToUpper(line); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250 stub")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				say("354 go")
				body, _ := reader.ReadDotBytes()
				got <- string(body)
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestMailSendsOverSMTP(testState *testing.T) {
	addr, got := smtpStub(testState)
	host, port, _ := net.SplitHostPort(addr)
	var portNum int
	fmt.Sscan(port, &portNum)
	mailer := &Mail{Host: host, Port: portNum, TLS: false, From: "bot@x.com", To: "alerts@example.com"}
	if err := mailer.SendFailure(context.Background(), failedReport(), "the log"); err != nil {
		testState.Fatal(err)
	}
	select {
	case data := <-got:
		if !strings.Contains(data, "Subject: [backstash] backup FAILED") || !strings.Contains(data, "run.log") {
			testState.Errorf("unexpected message:\n%s", data)
		}
	case <-time.After(3 * time.Second):
		testState.Fatal("no message received")
	}
}

func TestMailSkipsSuccessfulRuns(testState *testing.T) {
	// Host is unreachable on purpose: a success must return before any connection.
	if err := (&Mail{Host: "127.0.0.1", Port: 1}).SendFailure(context.Background(), okReport(), ""); err != nil {
		testState.Fatal(err)
	}
}
