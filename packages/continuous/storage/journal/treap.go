package journal

import (
	"hash/fnv"
)

// recordRef locates the current version of a record inside commits.log: the
// payload bytes of its value within a frame. Values are never held in memory
// by the index; readers fetch them positionally.
type recordRef struct {
	offset int64
	length int32
}

// treap is a persistent (immutable, path-copying) balanced tree keyed by
// record key. Every mutation returns a new root sharing untouched subtrees
// with the old one, so a snapshot is a root pointer and costs nothing to
// take or keep, and the writer's updates are O(ops * log n). Memory is one
// node per live record: key bytes plus a reference, not the value.
type treap struct {
	key         string
	ref         recordRef
	priority    uint32
	left, right *treap
	size        int
}

func priorityOf(key string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(key))
	return h.Sum32()
}

func treapSize(t *treap) int {
	if t == nil {
		return 0
	}
	return t.size
}

func newNode(key string, ref recordRef, left, right *treap) *treap {
	return &treap{key: key, ref: ref, priority: priorityOf(key), left: left, right: right, size: 1 + treapSize(left) + treapSize(right)}
}

func (t *treap) get(key string) (recordRef, bool) {
	for t != nil {
		switch {
		case key < t.key:
			t = t.left
		case key > t.key:
			t = t.right
		default:
			return t.ref, true
		}
	}
	return recordRef{}, false
}

// split returns trees with keys < key and keys >= key.
func split(t *treap, key string) (*treap, *treap) {
	if t == nil {
		return nil, nil
	}
	if key <= t.key {
		l, r := split(t.left, key)
		return l, newNode(t.key, t.ref, r, t.right)
	}
	l, r := split(t.right, key)
	return newNode(t.key, t.ref, t.left, l), r
}

// merge joins trees where every key of a is below every key of b.
func merge(a, b *treap) *treap {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case a.priority > b.priority:
		return newNode(a.key, a.ref, a.left, merge(a.right, b))
	default:
		return newNode(b.key, b.ref, merge(a, b.left), b.right)
	}
}

func (t *treap) put(key string, ref recordRef) *treap {
	l, r := split(t, key)
	// Drop an existing node with the same key from r.
	if r != nil {
		_, rest := split(r, key+"\x00")
		if r.size != treapSize(rest) {
			r = rest
		}
	}
	return merge(merge(l, newNode(key, ref, nil, nil)), r)
}

func (t *treap) remove(key string) *treap {
	l, r := split(t, key)
	_, rest := split(r, key+"\x00")
	return merge(l, rest)
}

// ascend visits keys >= from in order until visit returns false.
func (t *treap) ascend(from string, visit func(key string, ref recordRef) bool) bool {
	if t == nil {
		return true
	}
	if from <= t.key {
		if !t.left.ascend(from, visit) {
			return false
		}
		if !visit(t.key, t.ref) {
			return false
		}
	}
	return t.right.ascend(from, visit)
}
