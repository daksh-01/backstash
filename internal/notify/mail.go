package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"backstash/internal/report"
)

// Mail sends the failure email: a plain-text summary with the run log attached.
type Mail struct {
	Host     string
	Port     int
	TLS      bool // STARTTLS (port 465 always uses implicit TLS)
	User     string
	Password string
	From     string
	To       string
}

// oneLine keeps header values on one line so nothing can inject extra headers.
func oneLine(text string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(text)
}

func subject(rep report.Report) string {
	kinds := make([]string, 0, 3)
	for _, kind := range rep.FailedKinds() {
		kinds = append(kinds, string(kind))
	}
	subjectText := fmt.Sprintf("[backstash] %s FAILED: %s/%s", strings.ToLower(string(rep.Action)), rep.App, rep.Env)
	if len(kinds) > 0 {
		subjectText += " (" + strings.Join(kinds, ", ") + ")"
	}
	return oneLine(subjectText)
}

func body(rep report.Report) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s failed for %s (%s) on %s after %s.\n\n", rep.Action, rep.App, rep.Env, rep.Host, report.FormatDuration(rep.Duration))
	if rep.Err != nil {
		fmt.Fprintf(&builder, "Error: %s\n\n", rep.Err)
	}
	for _, targetItem := range rep.Targets {
		if targetItem.Err != nil {
			fmt.Fprintf(&builder, "[%s] FAILED: %s\n", targetItem.Kind, targetItem.Err)
		} else {
			fmt.Fprintf(&builder, "[%s] ok: %s (%s)\n", targetItem.Kind, targetItem.Name, targetItem.Size.String())
		}
	}
	builder.WriteString("\nThe full run log is attached as run.log.\n")
	return builder.String()
}

// BuildMail renders the MIME message (exported for tests).
func (mailer *Mail) BuildMail(rep report.Report, runLog string, now time.Time) []byte {
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	fmt.Fprintf(&out, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\n",
		oneLine(mailer.sender()), oneLine(mailer.To), subject(rep), now.Format(time.RFC1123Z))
	fmt.Fprintf(&out, "Content-Type: multipart/mixed; boundary=%s\r\n\r\n", writer.Boundary())

	text, _ := writer.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {`text/plain; charset="utf-8"`},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	qp := quotedprintable.NewWriter(text)
	_, _ = qp.Write([]byte(body(rep)))
	_ = qp.Close()

	att, _ := writer.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {`text/plain; charset="utf-8"; name="run.log"`},
		"Content-Disposition":       {`attachment; filename="run.log"`},
		"Content-Transfer-Encoding": {"base64"},
	})
	enc := base64.StdEncoding.EncodeToString([]byte(runLog))
	for len(enc) > 76 {
		_, _ = att.Write([]byte(enc[:76] + "\r\n"))
		enc = enc[76:]
	}
	_, _ = att.Write([]byte(enc + "\r\n"))
	_ = writer.Close()
	return out.Bytes()
}

func (mailer *Mail) sender() string {
	if mailer.From != "" {
		return mailer.From
	}
	return mailer.User
}

// connect opens an SMTP session: implicit TLS on 465, STARTTLS elsewhere when enabled, then auth.
func (mailer *Mail) connect(ctx context.Context) (*smtp.Client, error) {
	port := mailer.Port
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(mailer.Host, strconv.Itoa(port))
	dialer := net.Dialer{Timeout: 15 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("email: connect to %s: %w", addr, err)
	}
	tlsConf := &tls.Config{ServerName: mailer.Host, MinVersion: tls.VersionTLS12}
	if port == 465 {
		conn = tls.Client(conn, tlsConf)
	}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))

	client, err := smtp.NewClient(conn, mailer.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("email: %w", err)
	}
	if port != 465 && mailer.TLS {
		if err := client.StartTLS(tlsConf); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("email: starttls: %w", err)
		}
	}
	if mailer.User != "" {
		if err := client.Auth(smtp.PlainAuth("", mailer.User, mailer.Password, mailer.Host)); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("email: auth: %w", err)
		}
	}
	return client, nil
}

// SendFailure emails a failed report with the log attached. A successful report sends nothing.
func (mailer *Mail) SendFailure(ctx context.Context, rep report.Report, runLog string) error {
	if !rep.Failed() {
		return nil
	}
	var recipients []string
	for _, recipient := range strings.Split(mailer.To, ",") {
		recipient = strings.TrimSpace(recipient)
		if recipient != "" {
			recipients = append(recipients, recipient)
		}
	}
	if len(recipients) == 0 {
		return errors.New("email: no valid recipient addresses in alert_email_to")
	}

	client, err := mailer.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// Close client if ctx is cancelled during SMTP commands
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-done:
		}
	}()

	if err := client.Mail(mailer.sender()); err != nil {
		return fmt.Errorf("email: %w", err)
	}
	// RCPT TO accepts only one address at a time, so loop over comma-separated entries.
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			return fmt.Errorf("email: %w", err)
		}
	}
	dataWriter, err := client.Data()
	if err != nil {
		return fmt.Errorf("email: %w", err)
	}
	if _, err := dataWriter.Write(mailer.BuildMail(rep, runLog, time.Now())); err != nil {
		return fmt.Errorf("email: %w", err)
	}
	if err := dataWriter.Close(); err != nil {
		return fmt.Errorf("email: %w", err)
	}
	return client.Quit()
}
