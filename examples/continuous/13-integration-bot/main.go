// An integration bot over the host protocol. It uses stable request IDs so a
// redelivered webhook or a retried call returns the original submission
// instead of asking the model twice, and it observes the answer through a
// watch rather than polling.
package main

import (
	"context"
	"fmt"
	"net"

	"github.com/patriceckhart/zot/examples/continuous/internal/synthetic"
	"github.com/patriceckhart/zot/packages/continuous"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := synthetic.OpenStore(ctx, synthetic.TempStore("bot"))
	synthetic.Must(err)
	defer rt.Close()
	engine := synthetic.Engine(synthetic.NewClient())
	host, err := continuous.NewHost(rt, engine, continuous.HostOptions{})
	synthetic.Must(err)
	go host.Run(ctx)
	server := &continuous.HostServer{Host: host, Engine: engine, Tokens: map[string]continuous.Role{"bot-token-0123456789": continuous.RoleSubmit}}
	a, b := net.Pipe()
	go server.ServeConn(ctx, b)

	client, err := continuous.NewClient(ctx, a, "bot-token-0123456789")
	synthetic.Must(err)
	defer client.Close()

	var conv continuous.Conversation
	synthetic.Must(client.CallInto(ctx, "conversation.create", map[string]any{"workspace": "chat:room-42"}, &conv))

	// The webhook delivers the same message twice (same external message ID).
	submit := func() continuous.Submission {
		var s continuous.Submission
		synthetic.Must(client.CallInto(ctx, "conversation.submit", map[string]any{"id": conv.ID, "content": "what is the status?", "request_id": "msg-9001"}, &s))
		return s
	}
	first := submit()
	second := submit()
	fmt.Println("same submission for the redelivered message:", first.ID == second.ID)

	var settled continuous.Submission
	synthetic.Must(client.CallInto(ctx, "submission.wait", map[string]any{"id": first.ID}, &settled))
	var snap continuous.ConversationSnapshot
	synthetic.Must(client.CallInto(ctx, "conversation.snapshot", map[string]any{"id": conv.ID, "limit": 5}, &snap))
	fmt.Printf("submission %s, answer: %s\n", settled.State, snap.Entries[len(snap.Entries)-1].Content)

	// Different content under the same request ID is refused, not merged.
	err = client.CallInto(ctx, "conversation.submit", map[string]any{"id": conv.ID, "content": "something else", "request_id": "msg-9001"}, nil)
	fmt.Println("conflicting payload refused:", err)
}
