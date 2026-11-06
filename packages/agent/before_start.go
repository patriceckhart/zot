package agent

import (
	"context"
	"crypto/rand"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/core"
)

func wireBeforeAgentStart(ag *core.Agent, mgr *extensions.Manager, provider string) {
	runtimeSession := rand.Text()
	ag.BeforeStart = func(ctx context.Context, system string) string {
		session := ag.SessionID
		if session == "" {
			session, _ = mgr.SessionContext()
			if session == "" {
				session = runtimeSession
			}
		}
		return mgr.InterceptBeforeAgentStart(ctx, extproto.EventInterceptFromHost{
			SystemPrompt: &system, SessionID: session, SessionAlias: ag.SessionAlias, AgentRunID: rand.Text(),
			Provider: provider, Model: ag.Model,
		})
	}
}
