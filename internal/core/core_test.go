package core

import (
	"os"
	"testing"
	"time"

	"github.com/patriceckhart/zot/internal/provider"
)

func TestSessionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("ZOT_HOME", dir)

	sess, err := NewSession(dir, "/tmp/project", "anthropic", "claude-sonnet-4-5", "test")
	if err != nil {
		t.Fatal(err)
	}
	msg := provider.Message{
		Role: provider.RoleUser,
		Content: []provider.Content{
			provider.TextBlock{Text: "hello"},
		},
		Time: time.Now().UTC(),
	}
	if err := sess.AppendMessage(msg); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	_, msgs, err := OpenSession(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages", len(msgs))
	}
	tb, ok := msgs[0].Content[0].(provider.TextBlock)
	if !ok || tb.Text != "hello" {
		t.Fatalf("got %+v", msgs[0])
	}
}

func TestCostAdd(t *testing.T) {
	var c CostTracker
	c.Add(provider.Usage{InputTokens: 100, OutputTokens: 50, CostUSD: 0.01})
	c.Add(provider.Usage{InputTokens: 200, OutputTokens: 25, CostUSD: 0.02})
	if c.Total.InputTokens != 300 || c.Total.OutputTokens != 75 {
		t.Fatalf("got %+v", c.Total)
	}
	if c.Total.CostUSD < 0.0299 || c.Total.CostUSD > 0.0301 {
		t.Fatalf("got cost %v", c.Total.CostUSD)
	}
}
