package agent

import (
	"context"
	"crypto/rand"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func wireBeforeAgentStart(ag *core.Agent, mgr *extensions.Manager, providerID string) {
	runtimeSession := rand.Text()
	ag.BeforeRequest = func(ctx context.Context, purpose string, req provider.Request) (provider.Request, error) {
		session := req.SessionID
		if session == "" {
			session, _ = mgr.SessionContext()
			if session == "" {
				session = runtimeSession
			}
		}
		p := providerID
		if p == "" {
			p = ag.Client.Name()
		}
		return mgr.TransformRequest(ctx, session, p, purpose, req)
	}
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
			Provider: providerID, Model: ag.Model,
		})
	}
}
