// Package runlog captures everything logged during one run so a failure email can
// attach it, while still writing each line to stderr (cron's log file).
package runlog

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
)

// Capture is a slog handler that tees records to stderr and an in-memory buffer.
type Capture struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	handler slog.Handler
}

// New returns a Capture that also writes text records to out.
func New(out io.Writer) *Capture {
	cap := &Capture{}
	cap.handler = slog.NewTextHandler(io.MultiWriter(out, &lockedWriter{capture: cap}), &slog.HandlerOptions{Level: slog.LevelDebug})
	return cap
}

// Logger is a slog.Logger writing through the capture.
func (cap *Capture) Logger() *slog.Logger { return slog.New(cap) }

// Text returns everything logged so far.
func (cap *Capture) Text() string {
	cap.mu.Lock()
	defer cap.mu.Unlock()
	return cap.buf.String()
}

// lockedWriter serializes writes to the in-memory buffer using the Capture's mutex.
type lockedWriter struct{ capture *Capture }

func (writer *lockedWriter) Write(payload []byte) (int, error) {
	writer.capture.mu.Lock()
	defer writer.capture.mu.Unlock()
	return writer.capture.buf.Write(payload)
}

func (cap *Capture) Enabled(ctx context.Context, level slog.Level) bool {
	return cap.handler.Enabled(ctx, level)
}
func (cap *Capture) Handle(ctx context.Context, record slog.Record) error {
	return cap.handler.Handle(ctx, record)
}
func (cap *Capture) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &derived{capture: cap, handler: cap.handler.WithAttrs(attrs)}
}
func (cap *Capture) WithGroup(name string) slog.Handler {
	return &derived{capture: cap, handler: cap.handler.WithGroup(name)}
}

// derived is a child handler produced by WithAttrs or WithGroup. It delegates
// to its own slog handler but writes captured output into the parent's buffer.
type derived struct {
	capture *Capture
	handler slog.Handler
}

func (der *derived) Enabled(ctx context.Context, level slog.Level) bool {
	return der.handler.Enabled(ctx, level)
}
func (der *derived) Handle(ctx context.Context, record slog.Record) error {
	return der.handler.Handle(ctx, record)
}
func (der *derived) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &derived{capture: der.capture, handler: der.handler.WithAttrs(attrs)}
}
func (der *derived) WithGroup(name string) slog.Handler {
	return &derived{capture: der.capture, handler: der.handler.WithGroup(name)}
}
