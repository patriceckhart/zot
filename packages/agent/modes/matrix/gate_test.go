package matrix

import (
	"testing"

	"github.com/patriceckhart/zot/packages/agent/modes/bot"
)

func TestParseCommand(t *testing.T) {
	cases := map[string]bot.Command{
		"/start":  bot.CmdStart,
		"/help":   bot.CmdHelp,
		"/status": bot.CmdStatus,
		"/stop":   bot.CmdStop,
		"stop":    bot.CmdStop, // plain stop via bot.IsStopCommand
		"STOP":    bot.CmdStop,
	}
	for body, want := range cases {
		got, ok := parseCommand(body)
		if !ok || got != want {
			t.Fatalf("%q: got (%v,%v) want %v", body, got, ok, want)
		}
	}
	if _, ok := parseCommand("hello world"); ok {
		t.Fatal("plain text must not parse as a command")
	}
}

func TestGateInbound(t *testing.T) {
	self := "@zot:hs"
	paired := "@you:hs"

	// Self-echo skipped.
	if d := gateInbound(self, paired, self, 2, "hi", "zot"); d.action != gateIgnore {
		t.Fatal("self echo must be ignored")
	}
	// Unpaired DM → pair attempt.
	if d := gateInbound(self, "", "@new:hs", 2, "hi", "zot"); d.action != gatePair {
		t.Fatal("first DM sender should claim pairing")
	}
	// Wrong user rejected with reply.
	if d := gateInbound(self, paired, "@evil:hs", 2, "hi", "zot"); d.action != gateReject {
		t.Fatal("non-paired sender must be rejected")
	}
	// Paired DM accepted.
	if d := gateInbound(self, paired, paired, 2, "hi", "zot"); d.action != gateAccept || d.body != "hi" {
		t.Fatal("paired DM must be accepted")
	}
	// Group without mention: quiet.
	if d := gateInbound(self, paired, paired, 5, "hi all", "zot"); d.action != gateIgnore {
		t.Fatal("group message without mention must be ignored")
	}
	// Group with MXID mention: accepted, mention stripped.
	d := gateInbound(self, paired, paired, 5, "@zot:hs do the thing", "zot")
	if d.action != gateAccept || d.body != "do the thing" {
		t.Fatalf("group mention: %#v", d)
	}
	// Group with display-name mention.
	d = gateInbound(self, paired, paired, 5, "zot: do it", "zot")
	if d.action != gateAccept || d.body != "do it" {
		t.Fatalf("display-name mention: %#v", d)
	}
}
