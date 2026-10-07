package agent

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage/memory"
	"github.com/patriceckhart/zot/packages/core"
)

func TestAttachedTUIUsesHostDefaults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt, err := continuous.New(memory.Open())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	engine := continuous.EngineFunc(func(context.Context, continuous.Conversation) (*core.Agent, error) {
		return core.NewAgent(nil, "host-model", "", core.NewRegistry()), nil
	})
	host, err := continuous.NewHost(rt, engine, continuous.HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defaults := continuous.AgentConfig{Provider: "anthropic", Model: "host-model", Reasoning: "high"}
	server := &continuous.HostServer{Host: host, Engine: engine, DefaultConfig: defaults}
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, ln) }()
	defer func() { cancel(); <-served }()
	// These local defaults must not be sent to a differently configured host.
	local := Resolved{Provider: "openai", Model: "local-model", Reasoning: "low", CWD: "local-cwd"}
	for _, tc := range []struct {
		name string
		args Args
		want continuous.AgentConfig
	}{
		{"default", Args{}, defaults},
		{"explicit", Args{Provider: "OpenAI", Model: "local-model", Reasoning: "low"}, continuous.AgentConfig{Provider: "openai", Model: "local-model", Reasoning: "low"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args
			args.Continuous = ln.Addr().String()
			args.ContinuousWorkspace = tc.name
			_, _, _, closeAttached, err := attachContinuous(ctx, args, local, core.NewAgent(nil, "", "", core.NewRegistry()), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeAttached()
			conv, err := rt.OpenRoot(ctx, tc.name, continuous.AgentConfig{})
			if err != nil {
				t.Fatal(err)
			}
			if conv.Config.Provider != tc.want.Provider || conv.Config.Model != tc.want.Model || conv.Config.Reasoning != tc.want.Reasoning {
				t.Fatalf("root config=%+v want=%+v", conv.Config, tc.want)
			}
		})
	}
}
