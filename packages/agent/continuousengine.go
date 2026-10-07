package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/core"
)

// loadContinuousEngine stages and validates a fresh generation. It never
// reloads an existing manager, so a failed load leaves older engines intact.
func loadContinuousEngine(ctx context.Context, args Args, rt *continuous.Runtime, service func() *continuous.Service) (continuous.Engine, func(), error) {
	r, err := Resolve(args, true)
	if err != nil {
		return nil, nil, err
	}
	mgr, stop, errs := loadNonInteractiveExtensions(ctx, args, &r, "continuous")
	if err := errors.Join(errs...); err != nil {
		stop()
		return nil, nil, fmt.Errorf("load continuous extensions: %w", err)
	}
	engine := newContinuousEngine(r, mgr, rt, service)
	if _, err := engine.Build(ctx, continuous.Conversation{ID: "reload-probe"}); err != nil {
		stop()
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		stop()
		return nil, nil, err
	}
	return engine, stop, nil
}

// newContinuousEngine captures one immutable host configuration and extension
// manager. Every build gets its own registry, since tasks build agents in parallel.
// The CLI host supports one provider. Refuse mismatches rather than sending a
// conversation's model to the wrong provider with the host's credentials.
func newContinuousEngine(resolved Resolved, mgr *extensions.Manager, rt *continuous.Runtime, service func() *continuous.Service) continuous.Engine {
	return continuous.EngineFunc(func(ctx context.Context, c continuous.Conversation) (*core.Agent, error) {
		if c.Config.Provider != "" && canonicalProvider(c.Config.Provider) != resolved.Provider {
			return nil, fmt.Errorf("conversation provider %q does not match host provider %q", c.Config.Provider, resolved.Provider)
		}
		local := resolved
		local.ToolRegistry = make(core.Registry, len(resolved.ToolRegistry)+2)
		for name, tool := range resolved.ToolRegistry {
			local.ToolRegistry[name] = tool
		}
		ag := local.NewAgent()
		wireNonInteractiveAgentExtHooks(ctx, ag, mgr)
		ag.Tools["subagent"] = &continuous.SubagentTool{Runtime: rt, Service: service()}
		ag.Tools["handoff"] = continuous.HandoffTool{}
		return ag, nil
	})
}
