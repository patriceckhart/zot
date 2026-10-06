// A transactional todo document with revision checks, and a fork that reads
// the document as it was at the fork point while the parent moves on.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/patriceckhart/zot/examples/continuous/internal/synthetic"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage"
)

type todo struct {
	Items []string `json:"items"`
}

func main() {
	ctx := context.Background()
	rt, err := synthetic.OpenStore(ctx, synthetic.TempStore("todo"))
	synthetic.Must(err)
	defer rt.Close()

	docs := continuous.DocumentRegistry{}
	synthetic.Must(docs.Register(continuous.DocumentDefinition{
		Kind: "todo", Version: 1, Scope: "conversation",
		Initial: func() any { return todo{} },
		Validate: func(v json.RawMessage) error {
			var t todo
			return json.Unmarshal(v, &t)
		},
		Fork:    "as_of",
		History: true,
	}))

	c, err := rt.OpenRoot(ctx, "planning", continuous.AgentConfig{})
	synthetic.Must(err)
	doc, err := rt.ReadDocument(ctx, docs, "todo", c.ID)
	synthetic.Must(err)
	fmt.Printf("initial: %s (revision %d)\n", doc.Value, doc.Revision)

	doc, err = rt.WriteDocument(ctx, docs, "todo", c.ID, doc.Revision, todo{Items: []string{"write spec"}})
	synthetic.Must(err)
	// A stale writer loses: revision checks prevent lost updates.
	_, err = rt.WriteDocument(ctx, docs, "todo", c.ID, doc.Revision-1, todo{Items: []string{"clobber"}})
	fmt.Println("stale write rejected:", errors.Is(err, continuous.ErrDocumentConflict) || errors.Is(err, storage.ErrConflict))

	// Document history is indexed by the conversation's entry sequence, so
	// a fork point is an entry. Add one, fork there, then let the parent
	// move on with another entry and another write.
	// An input is answered before the fork: a conversation with an active
	// run cannot be forked.
	svc, err := continuous.NewService(rt, synthetic.Engine(synthetic.NewClient(synthetic.Step{Text: "Monday: spec."}, synthetic.Step{Text: "Tuesday: code."})), continuous.ExecutionOptions{})
	synthetic.Must(err)
	_, err = rt.Submit(ctx, c.ID, "user", "", "plan the week")
	synthetic.Must(err)
	_, _, err = svc.Step(ctx, c.ID)
	synthetic.Must(err)
	cur, _ := rt.Conversation(ctx, c.ID)
	fork, err := rt.Fork(ctx, c.ID, cur.EntrySequence, nil)
	synthetic.Must(err)
	_, err = rt.Submit(ctx, c.ID, "user", "", "add implementation")
	synthetic.Must(err)
	_, _, err = svc.Step(ctx, c.ID)
	synthetic.Must(err)
	_, err = rt.WriteDocument(ctx, docs, "todo", c.ID, doc.Revision, todo{Items: []string{"write spec", "implement"}})
	synthetic.Must(err)
	parentDoc, _ := rt.ReadDocument(ctx, docs, "todo", c.ID)
	forkDoc, err := rt.ReadDocument(ctx, docs, "todo", fork.ID)
	synthetic.Must(err)
	fmt.Printf("parent now: %s\n", parentDoc.Value)
	fmt.Printf("fork sees the value as of the fork: %s\n", forkDoc.Value)
}
