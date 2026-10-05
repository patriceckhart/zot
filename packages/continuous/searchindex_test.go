package continuous

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// The index returns exactly what the sequential scan returns for every
// text query, including type filters, time ranges, cursors, ancestry, and
// multi-word queries, and follows new commits incrementally. A rebuilt
// index is identical.
func TestSearchIndexMatchesSequentialScan(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{{text: "alpha answer"}, {text: "beta answer about alpha"}, {text: "gamma answer"}, {text: "delta"}})
	defer h.r.Close()
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "a", "", "alpha question")
	h.svc.Step(ctx, c.ID)
	idx, err := NewSearchIndex(ctx, h.r)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Entries() != 2 {
		t.Fatalf("indexed entries: %d", idx.Entries())
	}
	if _, err := h.r.Reset(ctx, c.ID, "fresh start, alpha gone"); err != nil {
		t.Fatal(err)
	}
	h.r.Submit(ctx, c.ID, "a", "", "beta question")
	h.svc.Step(ctx, c.ID)
	fork, err := h.r.Fork(ctx, c.ID, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.r.Submit(ctx, fork.ID, "a", "", "gamma question about ALPHA")
	h.svc.Step(ctx, fork.ID)
	queries := []SearchQuery{
		{ConversationID: c.ID, Text: "alpha"},
		{ConversationID: c.ID, Text: "ALPHA answer"},
		{ConversationID: c.ID, Text: "alpha", Types: []string{"assistant"}},
		{ConversationID: c.ID, Text: "alpha", Limit: 1},
		{ConversationID: c.ID, Text: "answer", After: time.Now().Add(-time.Hour)},
		{ConversationID: c.ID, Text: "answer", Before: time.Now().Add(-time.Hour)},
		{ConversationID: c.ID, Text: "nothing-here"},
		{ConversationID: fork.ID, Text: "alpha"},
		{ConversationID: fork.ID, Text: "alpha", Ancestry: true},
		{ConversationID: fork.ID, Text: "answer", Ancestry: true, Limit: 2},
		{ConversationID: c.ID, Text: "a"},
		{ConversationID: c.ID, Text: "lph"},
		{ConversationID: c.ID, Text: "pha answ"},
		{ConversationID: c.ID, Text: "ha ans"},
		{ConversationID: fork.ID, Text: "out ALPH", Ancestry: true},
		{ConversationID: c.ID},
	}
	check := func(label string, q SearchQuery) SearchResult {
		t.Helper()
		want, err := h.r.Search(ctx, q)
		if err != nil {
			t.Fatalf("%s scan: %v", label, err)
		}
		got, err := h.r.SearchIndexed(ctx, idx, q)
		if err != nil {
			t.Fatalf("%s indexed: %v", label, err)
		}
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		if !reflect.DeepEqual(wantJSON, gotJSON) {
			t.Fatalf("%s mismatch\nscan:  %s\nindex: %s", label, wantJSON, gotJSON)
		}
		return got
	}
	for i, q := range queries {
		got := check("query", q)
		// Paginate through the rest with the cursor as well.
		for got.More {
			q.Cursor = got.Next
			got = check("page", q)
			if i > 100 {
				t.Fatal("pagination did not end")
			}
		}
	}
	// The index followed the commits after its construction incrementally.
	status, _ := h.r.Status(ctx)
	if idx.Revision() != status.Revision || idx.Entries() != 7 {
		t.Fatalf("index revision %d entries %d, store %d", idx.Revision(), idx.Entries(), status.Revision)
	}
	// Multi-word queries intersect postings to select candidates, then
	// apply the same substring match as the scan: "beta alpha" is not a
	// substring of "beta answer about alpha", "about alpha" is.
	if both, _ := h.r.SearchIndexed(ctx, idx, SearchQuery{ConversationID: c.ID, Text: "beta alpha"}); len(both.Hits) != 0 {
		t.Fatalf("substring semantics lost: %+v", both.Hits)
	}
	phrase, _ := h.r.SearchIndexed(ctx, idx, SearchQuery{ConversationID: c.ID, Text: "about alpha"})
	if len(phrase.Hits) != 1 || phrase.Hits[0].Entry.Content != "beta answer about alpha" {
		t.Fatalf("phrase: %+v", phrase.Hits)
	}
	if seqs, ok := idx.candidates(c.ID, "beta alpha"); !ok || len(seqs) != 1 {
		t.Fatalf("candidates: %v %v", seqs, ok)
	}
	// Substring queries are answered by the index (trigrams), short words
	// fall back to the scan.
	if seqs, ok := idx.candidates(c.ID, "lph"); !ok || len(seqs) != 4 {
		t.Fatalf("substring candidates: %v %v", seqs, ok)
	}
	if _, ok := idx.candidates(c.ID, "ha ans"); ok {
		t.Fatal("short word answered by the index")
	}
	// A rebuilt index is equivalent.
	rebuilt, err := NewSearchIndex(ctx, h.r)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Entries() != idx.Entries() || rebuilt.Revision() != idx.Revision() {
		t.Fatalf("rebuilt index differs: %d/%d vs %d/%d", rebuilt.Entries(), rebuilt.Revision(), idx.Entries(), idx.Revision())
	}
	for _, q := range queries {
		idx = rebuilt
		check("rebuilt", q)
	}
	if _, err := h.r.SearchIndexed(ctx, idx, SearchQuery{ConversationID: "missing", Text: "x"}); err == nil {
		t.Fatal("missing conversation searched")
	}
	if a, b := tokenize("Hello, wörld! x 42"), map[string]struct{}{"hello": {}, "wörld": {}, "x": {}, "42": {}}; !reflect.DeepEqual(a, b) {
		t.Fatalf("tokenize: %v", a)
	}
	if g := trigrams("wörld"); !reflect.DeepEqual(g, []string{"wör", "örl", "rld"}) {
		t.Fatalf("trigrams: %v", g)
	}
}
