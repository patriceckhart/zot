package matrix

import (
	"strings"

	"github.com/patriceckhart/zot/packages/agent/modes/bot"
)

// parseCommand maps a message body onto a built-in bot command.
// Mirrors telegram's handleUpdate text switch + plain-stop detection.
func parseCommand(body string) (bot.Command, bool) {
	switch strings.TrimSpace(body) {
	case "/start":
		return bot.CmdStart, true
	case "/help":
		return bot.CmdHelp, true
	case "/status":
		return bot.CmdStatus, true
	case "/stop":
		return bot.CmdStop, true
	}
	if bot.IsStopCommand(body) {
		return bot.CmdStop, true
	}
	return 0, false
}

// gateDecision is the outcome of access-control on one inbound event.
type gateDecision struct {
	action gateAction
	body   string // cleaned body (mention stripped) when accepted
}

type gateAction int

const (
	gateIgnore gateAction = iota // silently drop
	gateReject                   // reply "paired with a different user"
	gateUnpaired                 // reply "not paired yet" (group / non-claim case)
	gatePair                     // claim pairing with this sender
	gateAccept                   // forward to the agent
)

// gateInbound applies the pairing + room-type rules from the design:
//   - self-echo skipped
//   - 1:1 DMs (memberCount == 2): first sender claims the bot; after
//     pairing only the allowed user is accepted
//   - group rooms: quiet unless directly mentioned by MXID or display
//     name; the mention prefix is stripped from the forwarded prompt
func gateInbound(selfID, allowedID, sender string, memberCount int, body, displayName string) gateDecision {
	if sender == selfID {
		return gateDecision{action: gateIgnore}
	}
	isDM := memberCount == 2
	if !isDM {
		cleaned, mentioned := stripMention(body, selfID, displayName)
		if !mentioned {
			return gateDecision{action: gateIgnore}
		}
		body = cleaned
	}
	if allowedID == "" {
		if isDM {
			return gateDecision{action: gatePair, body: body}
		}
		return gateDecision{action: gateUnpaired}
	}
	if sender != allowedID {
		return gateDecision{action: gateReject}
	}
	return gateDecision{action: gateAccept, body: body}
}

// stripMention removes a leading @mxid or display-name mention
// ("@zot:hs …", "zot: …", "zot …") and reports whether one was found.
func stripMention(body, selfID, displayName string) (string, bool) {
	trimmed := strings.TrimSpace(body)
	for _, prefix := range []string{selfID, displayName} {
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(trimmed, prefix) {
			rest := strings.TrimPrefix(trimmed, prefix)
			rest = strings.TrimLeft(rest, ":, ")
			return strings.TrimSpace(rest), true
		}
	}
	return body, false
}
