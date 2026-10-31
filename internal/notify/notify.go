// Package notify posts a short text message to a chat webhook. It knows the
// payload shapes of Slack and Discord incoming webhooks; any other URL gets a
// generic JSON body ({"text": …, plus whatever extra the caller passes}), which
// most automation tools (n8n, Zapier, Make, Power Automate) accept and can
// forward to email, LINE or anywhere else.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Kind is the payload shape for a webhook URL.
type Kind string

const (
	Slack   Kind = "slack"
	Discord Kind = "discord"
	Generic Kind = "generic"
)

// KindOf picks the payload shape from the URL's host.
func KindOf(raw string) Kind {
	u, err := url.Parse(raw)
	if err != nil {
		return Generic
	}
	h := strings.ToLower(u.Hostname())
	switch {
	case h == "hooks.slack.com":
		return Slack
	case (h == "discord.com" || h == "discordapp.com" || strings.HasSuffix(h, ".discord.com")) && strings.Contains(u.Path, "/api/webhooks/"):
		return Discord
	}
	return Generic
}

// discordLimit is Discord's maximum message length.
const discordLimit = 2000

// Send posts text to webhook. extra is merged into the generic payload only
// (e.g. structured changes for tools that want more than text).
func Send(ctx context.Context, webhook, text string, extra map[string]any) error {
	var body map[string]any
	switch KindOf(webhook) {
	case Slack:
		body = map[string]any{"text": text}
	case Discord:
		if len([]rune(text)) > discordLimit {
			text = string([]rune(text)[:discordLimit-1]) + "…"
		}
		body = map[string]any{"content": text}
	default:
		body = map[string]any{"text": text}
		for k, v := range extra {
			body[k] = v
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("webhook answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Redact hides the secret part of a webhook URL (its path) for logs.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(webhook)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}
