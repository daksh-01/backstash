// Package notify sends the Slack card for every run and the email for failed ones.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"backstash/internal/report"
)

const (
	successColor = "#2eb67d"
	failureColor = "#e01e5a"
	maxErrChars  = 500
	slackAPI     = "https://slack.com/api/chat.postMessage"
)

// Slack posts run cards with a bot token (`chat:write` scope).
type Slack struct {
	Token, Channel string
	URL            string // override in tests; default slack.com
	HTTP           *http.Client
	Retries        int
	Sleep          func(time.Duration)
}

// BlockType specifies the layout block type in Block Kit.
type BlockType string

const (
	BlockTypeHeader  BlockType = "header"
	BlockTypeSection BlockType = "section"
)

// TextType specifies the text formatting syntax.
type TextType string

const (
	TextTypePlainText TextType = "plain_text"
	TextTypeMrkdwn    TextType = "mrkdwn"
)

// TextElement is a formatted text component in a Slack block.
type TextElement struct {
	Type TextType `json:"type"`
	Text string   `json:"text"`
}

// Block is one Block Kit layout block.
type Block struct {
	Type   BlockType      `json:"type"`
	Text   *TextElement   `json:"text,omitempty"`
	Fields []*TextElement `json:"fields,omitempty"`
}

// Attachment holds a colored status bar and layout blocks.
type Attachment struct {
	Color  string  `json:"color"`
	Blocks []Block `json:"blocks"`
}

// Message is a Block Kit card: Text is the notification fallback.
type Message struct {
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments"`
}

type postMessageRequest struct {
	Channel     string       `json:"channel"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments"`
}

func escape(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

func field(label, value string) *TextElement {
	return &TextElement{Type: TextTypeMrkdwn, Text: "*" + label + "*\n" + escape(value)}
}

// truncateRunes shortens text to at most maxRunes Unicode code points (not bytes) so that
// multi-byte characters are never split.
func truncateRunes(text string, maxRunes int) string {
	runes := []rune(text)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return text
}

// BuildSlack renders the card: coloured bar, heading, details, then one line per target.
func BuildSlack(rep report.Report, now time.Time) Message {
	status := "Successful"
	color := successColor
	if rep.Failed() {
		status, color = "Failed", failureColor
	}
	title := fmt.Sprintf("%s %s", rep.Action, status)

	fields := []*TextElement{
		field("Application", rep.App), field("Environment", rep.Env), field("Host", rep.Host),
	}
	if !rep.Failed() && rep.TotalSize() > 0 {
		fields = append(fields, field("Size", rep.TotalSize().String()))
	}
	fields = append(fields, field("Duration", report.FormatDuration(rep.Duration)))
	if count := rep.TotalPruned(); count > 0 {
		fields = append(fields, field("Old backups removed", fmt.Sprint(count)))
	}
	fields = append(fields, field("Completed", now.UTC().Format("January 02, 2006, 15:04:05 UTC")))

	var lines []string
	if rep.Err != nil {
		lines = append(lines, "*Error*\n"+escape(truncateRunes(rep.Err.Error(), maxErrChars)))
	}
	for _, targetItem := range rep.Targets {
		switch {
		case targetItem.Err != nil:
			lines = append(lines, fmt.Sprintf(":x: *%s*: %s", targetItem.Kind, escape(truncateRunes(targetItem.Err.Error(), maxErrChars))))
		default:
			line := fmt.Sprintf(":white_check_mark: *%s*: %s (%s)", targetItem.Kind, escape(targetItem.Name), targetItem.Size.String())
			if targetItem.Note != "" {
				line += "\n:warning: " + escape(truncateRunes(targetItem.Note, maxErrChars))
			}
			lines = append(lines, line)
		}
	}
	blocks := []Block{
		{Type: BlockTypeHeader, Text: &TextElement{Type: TextTypePlainText, Text: title}},
		{Type: BlockTypeSection, Fields: fields},
	}
	if len(lines) > 0 {
		blocks = append(blocks, Block{
			Type: BlockTypeSection, Text: &TextElement{Type: TextTypeMrkdwn, Text: strings.Join(lines, "\n")},
		})
	}
	return Message{
		Text:        fmt.Sprintf("%s: %s (%s)", title, rep.App, rep.Env),
		Attachments: []Attachment{{Color: color, Blocks: blocks}},
	}
}

func (slack *Slack) httpClient() *http.Client {
	if slack.HTTP != nil {
		return slack.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (slack *Slack) apiURL() string {
	if slack.URL != "" {
		return slack.URL
	}
	return slackAPI
}

func (slack *Slack) sleepFn() func(time.Duration) {
	if slack.Sleep != nil {
		return slack.Sleep
	}
	return time.Sleep
}

// Notify posts the card. Callers log (not fail) on error: a Slack outage must not fail a backup.
func (slack *Slack) Notify(ctx context.Context, rep report.Report) error {
	msg := BuildSlack(rep, time.Now())
	body, err := json.Marshal(postMessageRequest{
		Channel:     slack.Channel,
		Text:        msg.Text,
		Attachments: msg.Attachments,
	})
	if err != nil {
		return err
	}

	client := slack.httpClient()
	targetURL := slack.apiURL()
	sleep := slack.sleepFn()

	for attempt := 0; ; attempt++ {
		retry, retryAfter, err := slack.post(ctx, client, targetURL, body)
		if err == nil || !retry || attempt >= slack.Retries {
			return err
		}
		delay := time.Duration(attempt+1) * time.Second
		if retryAfter > delay {
			delay = retryAfter
		}
		sleep(delay)
	}
}

// post sends the card once. retry is true for failures worth repeating: network errors,
// rate limits and server errors.
func (slack *Slack) post(ctx context.Context, client *http.Client, targetURL string, body []byte) (retry bool, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+slack.Token)
	res, err := client.Do(req)
	if err != nil {
		return true, 0, errors.New("slack request failed") // never echo the URL or token
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
		if res.StatusCode == http.StatusTooManyRequests {
			if retryHeader := res.Header.Get("Retry-After"); retryHeader != "" {
				if secs, parseErr := strconv.Atoi(retryHeader); parseErr == nil && secs > 0 {
					retryAfter = time.Duration(secs) * time.Second
				}
			}
		}
		return true, retryAfter, fmt.Errorf("slack returned HTTP %d", res.StatusCode)
	}
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || !out.OK {
		code := out.Error // e.g. channel_not_found; never the token
		if code == "" {
			code = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return false, 0, fmt.Errorf("slack notification failed: %s", code)
	}
	return false, 0, nil
}
