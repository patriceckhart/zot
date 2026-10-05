// Concurrent conversations (channels and threads) under one host, and a fork
// that shares history with its parent up to a committed entry.
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/patriceckhart/zot/examples/continuous/internal/synthetic"
	"github.com/patriceckhart/zot/packages/continuous"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := synthetic.OpenStore(ctx, synthetic.TempStore("threads"))
	synthetic.Must(err)
	defer rt.Close()

	host, err := continuous.NewHost(rt, synthetic.Engine(synthetic.NewClient()), continuous.HostOptions{})
	synthetic.Must(err)
	go host.Run(ctx)

	// Three independent channels run concurrently.
	var ids []string
	for _, name := range []string{"general", "deploys", "support"} {
		c, err := rt.OpenRoot(ctx, "channel:"+name, continuous.AgentConfig{Provider: "synthetic", Model: "synthetic-model"})
		synthetic.Must(err)
		ids = append(ids, c.ID)
		_, err = rt.Submit(ctx, c.ID, "bot", "", "hello from "+name)
		synthetic.Must(err)
	}
	host.Nudge()
	for _, id := range ids {
		waitIdle(ctx, rt, id)
		snap, _ := rt.ConversationSnapshot(ctx, id, 10)
		fmt.Printf("%s: %s\n", snap.Conversation.ID[:8], snap.Entries[len(snap.Entries)-1].Content)
	}

	// A thread forks the first channel after its first answer: it sees the
	// parent's history up to that entry, and the parent is never rewritten.
	parent, _ := rt.Conversation(ctx, ids[0])
	fork, err := rt.Fork(ctx, parent.ID, parent.EntrySequence, nil)
	synthetic.Must(err)
	_, err = rt.Submit(ctx, fork.ID, "bot", "", "thread question")
	synthetic.Must(err)
	host.Nudge()
	waitIdle(ctx, rt, fork.ID)
	result, err := rt.Search(ctx, continuous.SearchQuery{ConversationID: fork.ID, Ancestry: true})
	synthetic.Must(err)
	fmt.Println("fork history (inherited entries marked with the parent id):")
	for _, hit := range result.Hits {
		fmt.Printf("  %s#%d %-10s %s\n", hit.Owner[:8], hit.Sequence, hit.Entry.Type, hit.Entry.Content)
	}
	again, _ := rt.Conversation(ctx, parent.ID)
	fmt.Println("parent entries unchanged:", again.EntrySequence == parent.EntrySequence)
}

func waitIdle(ctx context.Context, rt *continuous.Runtime, id string) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := rt.ConversationSnapshot(ctx, id, 1)
		synthetic.Must(err)
		if len(snap.Queue) == 0 && (snap.Run == nil || snap.Run.Phase == "done") {
			return
		}
		rt.Wait(ctx, snap.Revision)
	}
	synthetic.Must(fmt.Errorf("conversation %s did not settle", id))
}
