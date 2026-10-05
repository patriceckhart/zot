package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// SearchIndex is an optional, rebuildable full-text projection over committed
// entries. It maps lowercase trigrams of word tokens to the entries that
// contain them, per owning conversation, and follows commits incrementally.
// Trigrams keep the index exact for substring queries: a query word that
// occurs inside an entry word has all its trigrams in that word. It is never
// authoritative: Search uses it to select candidate entries, then reads and
// filters each candidate from the committed snapshot with the same substring
// match as the sequential scan. An index that is behind the snapshot is
// topped up before use; a query the index cannot answer exactly (a word
// shorter than three characters) falls back to the sequential scan.
//
// The index lives in memory and is rebuilt from the store on construction.
// Its size is proportional to the distinct (trigram, entry) pairs in the
// store.
type SearchIndex struct {
	mu sync.RWMutex
	// revision is the last commit applied.
	revision uint64
	// postings maps conversation ID to trigram to ascending entry sequences.
	postings map[string]map[string][]uint64
	// entries counts indexed entries for metrics.
	entries int
}

// NewSearchIndex builds an index over the current committed state of r.
func NewSearchIndex(ctx context.Context, r *Runtime) (*SearchIndex, error) {
	idx := &SearchIndex{postings: map[string]map[string][]uint64{}}
	if err := idx.Rebuild(ctx, r); err != nil {
		return nil, err
	}
	return idx, nil
}

// Rebuild discards the index and reindexes every committed entry at one
// snapshot. It is the recovery path for an index that fell behind retained
// history.
func (idx *SearchIndex) Rebuild(ctx context.Context, r *Runtime) error {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	fresh := &SearchIndex{postings: map[string]map[string][]uint64{}}
	after := ""
	for {
		page, err := snap.Page("entry/", after, 500)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			after = row.Key
			if err := ctx.Err(); err != nil {
				return err
			}
			var e Entry
			if json.Unmarshal(row.Value, &e) != nil {
				return storage.ErrCorrupt
			}
			owner, seq, ok := parseEntryKey(row.Key)
			if !ok {
				return storage.ErrCorrupt
			}
			fresh.add(owner, seq, e)
		}
	}
	fresh.revision = snap.Revision()
	idx.mu.Lock()
	idx.postings, idx.entries, idx.revision = fresh.postings, fresh.entries, fresh.revision
	idx.mu.Unlock()
	return nil
}

// Follow applies commits after the index revision up to and including
// through. It returns storage.ErrCursor when the needed history is no longer
// retained; the caller should Rebuild.
func (idx *SearchIndex) Follow(ctx context.Context, r *Runtime, through uint64) error {
	for {
		idx.mu.RLock()
		cursor := idx.revision
		idx.mu.RUnlock()
		if cursor >= through {
			return nil
		}
		commits, err := r.store.Scan(ctx, cursor, 200)
		if err != nil {
			return err
		}
		if len(commits) == 0 {
			return nil
		}
		idx.mu.Lock()
		for _, cm := range commits {
			if cm.Revision <= idx.revision {
				continue
			}
			if cm.Revision > through {
				break
			}
			for _, op := range cm.Operations {
				if op.Delete || !strings.HasPrefix(op.Key, "entry/") {
					continue
				}
				owner, seq, ok := parseEntryKey(op.Key)
				if !ok {
					continue
				}
				var e Entry
				if json.Unmarshal(op.Value, &e) != nil {
					continue
				}
				idx.add(owner, seq, e)
			}
			idx.revision = cm.Revision
		}
		idx.mu.Unlock()
	}
}

// Revision is the last commit the index reflects.
func (idx *SearchIndex) Revision() uint64 {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.revision
}

// Entries is the number of indexed entries.
func (idx *SearchIndex) Entries() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.entries
}

// add indexes one entry. Entries are immutable, so a sequence already
// present is left alone.
func (idx *SearchIndex) add(owner string, seq uint64, e Entry) {
	tokens := map[string]struct{}{}
	for word := range tokenize(e.Content) {
		for _, gram := range trigrams(word) {
			tokens[gram] = struct{}{}
		}
	}
	if len(tokens) == 0 {
		return
	}
	byToken := idx.postings[owner]
	if byToken == nil {
		byToken = map[string][]uint64{}
		idx.postings[owner] = byToken
	}
	added := false
	for tok := range tokens {
		list := byToken[tok]
		if n := len(list); n > 0 && list[n-1] >= seq {
			// Out of order or duplicate: keep the list sorted and unique.
			i := sort.Search(n, func(i int) bool { return list[i] >= seq })
			if i < n && list[i] == seq {
				continue
			}
			list = append(list, 0)
			copy(list[i+1:], list[i:])
			list[i] = seq
		} else {
			list = append(list, seq)
		}
		byToken[tok] = list
		added = true
	}
	if added {
		idx.entries++
	}
}

// candidates returns the entry sequences of owner whose content contains
// every trigram of every word of the query, ascending: a superset of the
// substring matches. ok is false when the index cannot answer exactly (no
// word, or a word shorter than three characters), in which case the caller
// scans sequentially.
func (idx *SearchIndex) candidates(owner, text string) (seqs []uint64, ok bool) {
	grams := map[string]struct{}{}
	for word := range tokenize(text) {
		list := trigrams(word)
		if len(list) == 0 {
			return nil, false
		}
		for _, gram := range list {
			grams[gram] = struct{}{}
		}
	}
	if len(grams) == 0 {
		return nil, false
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	byToken := idx.postings[owner]
	var lists [][]uint64
	for gram := range grams {
		list := byToken[gram]
		if len(list) == 0 {
			return []uint64{}, true
		}
		lists = append(lists, list)
	}
	sort.Slice(lists, func(i, j int) bool { return len(lists[i]) < len(lists[j]) })
	result := append([]uint64(nil), lists[0]...)
	for _, list := range lists[1:] {
		result = intersectSorted(result, list)
		if len(result) == 0 {
			break
		}
	}
	return result, true
}

func intersectSorted(a, b []uint64) []uint64 {
	out := a[:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

// tokenize splits text into lowercase letter and digit runs. Query and
// content use the same split, so a query word that is a substring of the
// content lies within one content word.
func tokenize(text string) map[string]struct{} {
	out := map[string]struct{}{}
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out[b.String()] = struct{}{}
		}
		b.Reset()
	}
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// trigrams returns the distinct three-rune windows of a word, or nil for a
// word shorter than three runes.
func trigrams(word string) []string {
	runes := []rune(word)
	if len(runes) < 3 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(runes)-2)
	for i := 0; i+3 <= len(runes); i++ {
		gram := string(runes[i : i+3])
		if _, dup := seen[gram]; dup {
			continue
		}
		seen[gram] = struct{}{}
		out = append(out, gram)
	}
	return out
}

// parseEntryKey splits entry/<conversation>/<sequence>.
func parseEntryKey(key string) (string, uint64, bool) {
	rest := strings.TrimPrefix(key, "entry/")
	i := strings.LastIndexByte(rest, '/')
	if i <= 0 || i == len(rest)-1 {
		return "", 0, false
	}
	var seq uint64
	for _, c := range rest[i+1:] {
		if c < '0' || c > '9' {
			return "", 0, false
		}
		seq = seq*10 + uint64(c-'0')
	}
	return rest[:i], seq, seq > 0
}

// SearchIndexed is Search accelerated by idx. The index selects candidate
// entries for the text; every candidate is then read from the snapshot and
// filtered exactly as Search does, so results never depend on index
// freshness beyond the snapshot it was topped up to. When the index cannot
// follow the snapshot (history no longer retained), the query falls back to
// Search. Queries without text use Search directly.
func (r *Runtime) SearchIndexed(ctx context.Context, idx *SearchIndex, q SearchQuery) (SearchResult, error) {
	if idx == nil || strings.TrimSpace(q.Text) == "" {
		return r.Search(ctx, q)
	}
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.Limit > 1000 {
		return SearchResult{}, errors.New("search limit must be between 1 and 1000")
	}
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	if _, err := conversation(snap, q.ConversationID); err != nil {
		return SearchResult{}, err
	}
	if err := idx.Follow(ctx, r, snap.Revision()); err != nil {
		if errors.Is(err, storage.ErrCursor) {
			if err := idx.Rebuild(ctx, r); err != nil {
				return SearchResult{}, err
			}
		} else {
			return SearchResult{}, err
		}
	}
	if idx.Revision() < snap.Revision() {
		// The index could not catch up to this snapshot; the sequential
		// scan is exact.
		return r.Search(ctx, q)
	}
	// Segments of the visible history, root first, with their bounds.
	type segment struct {
		id  string
		end uint64
	}
	var chain []segment
	seen := map[string]bool{}
	id, end := q.ConversationID, uint64(0)
	for id != "" {
		if seen[id] {
			return SearchResult{}, errors.New("fork ancestry cycle")
		}
		seen[id] = true
		c, err := conversation(snap, id)
		if err != nil {
			return SearchResult{}, err
		}
		if end == 0 || end > c.EntrySequence {
			end = c.EntrySequence
		}
		chain = append(chain, segment{id: id, end: end})
		if c.Parent == nil || !q.Ancestry {
			break
		}
		id, end = c.Parent.ConversationID, c.Parent.At
	}
	types := map[string]bool{}
	for _, t := range q.Types {
		types[t] = true
	}
	needle := strings.ToLower(q.Text)
	matches := func(e Entry) bool {
		if len(types) > 0 && !types[e.Type] {
			return false
		}
		if !q.After.IsZero() || !q.Before.IsZero() {
			if e.Time.IsZero() || (!q.After.IsZero() && e.Time.Before(q.After)) || (!q.Before.IsZero() && !e.Time.Before(q.Before)) {
				return false
			}
		}
		return strings.Contains(strings.ToLower(e.Content), needle)
	}
	// A cursor names the last returned entry: later segments are fully in
	// range, the cursor's segment from the next sequence on, earlier
	// segments not at all. A cursor outside the visible history yields
	// nothing, like the sequential scan.
	cursorSegment := -1
	if q.Cursor.Owner != "" {
		for i := range chain {
			if chain[i].id == q.Cursor.Owner {
				cursorSegment = i
			}
		}
		if cursorSegment < 0 {
			return SearchResult{Revision: snap.Revision(), Hits: []SearchHit{}}, nil
		}
	}
	result := SearchResult{Revision: snap.Revision(), Hits: []SearchHit{}}
	for i := len(chain) - 1; i >= 0; i-- {
		if cursorSegment >= 0 && i > cursorSegment {
			continue
		}
		seg := chain[i]
		seqs, ok := idx.candidates(seg.id, q.Text)
		if !ok {
			return r.Search(ctx, q)
		}
		for _, seq := range seqs {
			if seq > seg.end {
				break
			}
			if i == cursorSegment && seq <= q.Cursor.Sequence {
				continue
			}
			if err := ctx.Err(); err != nil {
				return SearchResult{}, err
			}
			e, ok, err := read[Entry](snap, entryKey(seg.id, seq))
			if err != nil {
				return SearchResult{}, err
			}
			if !ok || !matches(e) {
				continue
			}
			if len(result.Hits) >= q.Limit {
				result.More = true
				return result, nil
			}
			result.Hits = append(result.Hits, SearchHit{Owner: seg.id, Sequence: seq, Entry: e})
			result.Next = SearchCursor{Owner: seg.id, Sequence: seq}
		}
	}
	return result, nil
}
