package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKindOf(t *testing.T) {
	cases := map[string]Kind{
		"https://hooks.slack.com/services/T0/B0/xyz":        Slack,
		"https://discord.com/api/webhooks/1/abc":            Discord,
		"https://discordapp.com/api/webhooks/1/abc":         Discord,
		"https://n8n.example.com/webhook/meterm":            Generic,
		"https://evil.example.com/hooks.slack.com/services": Generic,
	}
	for u, want := range cases {
		if got := KindOf(u); got != want {
			t.Errorf("KindOf(%s) = %s, want %s", u, got, want)
		}
	}
}

func TestSendGenericIncludesExtra(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	if err := Send(context.Background(), srv.URL, "hello", map[string]any{"changes": []int{1}}); err != nil {
		t.Fatal(err)
	}
	if got["text"] != "hello" || got["changes"] == nil {
		t.Errorf("payload = %v", got)
	}
}

func TestSendReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid_token", http.StatusForbidden)
	}))
	defer srv.Close()
	err := Send(context.Background(), srv.URL, "x", nil)
	if err == nil || !strings.Contains(err.Error(), "invalid_token") {
		t.Errorf("err = %v", err)
	}
}

func TestRedactHidesTheSecretPath(t *testing.T) {
	if r := Redact("https://hooks.slack.com/services/T0/B0/secret"); strings.Contains(r, "secret") {
		t.Errorf("Redact leaked: %s", r)
	}
}
