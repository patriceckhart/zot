package agent

import (
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// Persist at message boundaries, not prompt completion: a long-running tool
// must not hide already committed calls or results from session recovery.
func persistSwarmMessages(ag *core.Agent, sess *core.Session) {
	if sess == nil {
		return
	}
	previous := ag.OnMessageAppended
	ag.OnMessageAppended = func(msg provider.Message) {
		if previous != nil {
			previous(msg)
		}
		_ = sess.AppendMessage(msg)
	}
	compacted := ag.OnTranscriptCompacted
	ag.OnTranscriptCompacted = func(messages []provider.Message) {
		if compacted != nil {
			compacted(messages)
		}
		_ = sess.AppendCompaction(messages)
	}
}
