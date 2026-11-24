package run

import (
	"context"
	"log/slog"

	"backstash/internal/config"
	"backstash/internal/notify"
	"backstash/internal/report"
	"backstash/internal/secrets"
)

// Notifier fans a finished run out to Slack (every run) and email (failures). Either may be
// nil when it is not configured; both are skipped silently then.
type Notifier struct {
	Slack *notify.Slack
	Mail  *notify.Mail
	Log   *slog.Logger
}

// NewNotifier builds the notifiers that are fully configured.
func NewNotifier(cfg *config.Config, app *config.App, sec secrets.Set, log *slog.Logger) Notifier {
	notifier := Notifier{Log: log}
	channel := app.SlackChannel
	if channel == "" {
		channel = cfg.Host.Slack.Channel
	}
	if token, ok := sec.Get(SecretSlackToken); ok && channel != "" {
		notifier.Slack = &notify.Slack{Token: token, Channel: channel, Retries: 2}
	}
	if smtpConfig := cfg.Host.SMTP; smtpConfig.Host != "" && cfg.Host.AlertEmailTo != "" {
		user, _ := sec.Get(SecretSMTPUser)
		pass, _ := sec.Get(SecretSMTPPass)
		useTLS := smtpConfig.TLS == nil || *smtpConfig.TLS
		notifier.Mail = &notify.Mail{Host: smtpConfig.Host, Port: smtpConfig.Port, TLS: useTLS, User: user, Password: pass, From: smtpConfig.From, To: cfg.Host.AlertEmailTo}
	}
	return notifier
}

// Send alerts for a finished run. A notification failure is logged, never fatal.
func (notifier Notifier) Send(ctx context.Context, rep report.Report, runLog string) {
	if rep.Silent {
		return
	}
	if notifier.Slack != nil {
		if err := notifier.Slack.Notify(ctx, rep); err != nil {
			notifier.Log.Error("slack notification failed", "err", err)
		}
	}
	if notifier.Mail != nil && rep.Failed() {
		if err := notifier.Mail.SendFailure(ctx, rep, runLog); err != nil {
			notifier.Log.Error("failure email not sent", "err", err)
		}
	}
}
