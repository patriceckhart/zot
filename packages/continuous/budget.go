package continuous

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Budget is an explicit, conservative spending control over the usage
// ledger. It is checked before each model request: the known spend of the
// scope plus a reservation for the request must stay within the limit.
// Unknown-cost attempts count as the reservation, never as zero. Budgets
// bound what the runtime starts, not what a provider bills: delayed usage
// reports and unknown pricing allow bounded overshoot of at most one
// reservation per active conversation.
type Budget struct {
	// Scope is runtime or conversation. ConversationID is set for the latter.
	Scope          string `json:"scope"`
	ConversationID string `json:"conversation_id,omitempty"`
	// LimitUSD caps known plus reserved cost. Zero means no cost cap.
	LimitUSD float64 `json:"limit_usd,omitempty"`
	// LimitTokens caps known input plus output tokens. Zero means no cap.
	LimitTokens int `json:"limit_tokens,omitempty"`
	// ReserveUSD is the conservative per-request reservation. Zero uses
	// the runtime default. Unknown-cost attempts are charged this amount.
	ReserveUSD float64 `json:"reserve_usd,omitempty"`
	// Window, when positive, limits the budget to ledger rows newer than
	// now minus Window. Rows without a timestamp always count.
	Window   time.Duration `json:"window,omitempty"`
	Revision uint64        `json:"revision"`
}

// BudgetStatus is the projection a check produces.
type BudgetStatus struct {
	Budget      Budget  `json:"budget"`
	SpentUSD    float64 `json:"spent_usd"`
	ReservedUSD float64 `json:"reserved_usd"`
	Tokens      int     `json:"tokens"`
	Unknown     int     `json:"unknown"`
	// Exceeded explains the refusal, empty when within limits.
	Exceeded string `json:"exceeded,omitempty"`
}

var ErrBudgetExceeded = errors.New("continuous budget exceeded")

const defaultReserveUSD = 0.05

func budgetKey(scope, conversationID string) string {
	if scope == "runtime" {
		return "budget/runtime"
	}
	return "budget/conversation/" + conversationID
}

// SetBudget commits a budget with a revision check (0 creates). A zero
// LimitUSD and LimitTokens removes the budget.
func (r *Runtime) SetBudget(ctx context.Context, b Budget, expectedRevision uint64) (Budget, error) {
	if b.Scope != "runtime" && b.Scope != "conversation" {
		return Budget{}, fmt.Errorf("budget scope must be runtime or conversation")
	}
	if b.Scope == "conversation" && b.ConversationID == "" {
		return Budget{}, fmt.Errorf("conversation budget requires a conversation ID")
	}
	if b.LimitUSD < 0 || b.LimitTokens < 0 || b.ReserveUSD < 0 || b.Window < 0 {
		return Budget{}, fmt.Errorf("budget limits must not be negative")
	}
	key := budgetKey(b.Scope, b.ConversationID)
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Budget{}, err
		}
		if b.Scope == "conversation" {
			if _, err := conversation(snap, b.ConversationID); err != nil {
				return Budget{}, err
			}
		}
		current, ok, err := read[Budget](snap, key)
		if err != nil {
			return Budget{}, err
		}
		if (ok && current.Revision != expectedRevision) || (!ok && expectedRevision != 0) {
			return current, storage.ErrConflict
		}
		var ops []storage.Operation
		if b.LimitUSD == 0 && b.LimitTokens == 0 {
			if !ok {
				return Budget{}, nil
			}
			ops = append(ops, storage.Operation{Key: key, Delete: true})
		} else {
			b.Revision = snap.Revision() + 1
			ops = append(ops, record(key, b))
		}
		err = r.commit(ctx, snap, "budget.set", ops...)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return b, err
	}
}

// Budget reads the budget of a scope. ok is false when none is set.
func (r *Runtime) Budget(ctx context.Context, scope, conversationID string) (Budget, bool, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return Budget{}, false, err
	}
	return read[Budget](snap, budgetKey(scope, conversationID))
}

// CheckBudgets evaluates the conversation and runtime budgets for one more
// request of the conversation. It returns ErrBudgetExceeded with the first
// exceeded status when the request must not start.
func (r *Runtime) CheckBudgets(ctx context.Context, conversationID string) ([]BudgetStatus, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return checkBudgets(ctx, snap, conversationID, time.Now().UTC())
}

func checkBudgets(ctx context.Context, snap storage.Snapshot, conversationID string, now time.Time) ([]BudgetStatus, error) {
	var statuses []BudgetStatus
	for _, key := range []string{budgetKey("conversation", conversationID), budgetKey("runtime", "")} {
		b, ok, err := read[Budget](snap, key)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		status, err := budgetStatus(ctx, snap, b, now)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
		if status.Exceeded != "" {
			return statuses, fmt.Errorf("%w: %s", ErrBudgetExceeded, status.Exceeded)
		}
	}
	return statuses, nil
}

// budgetStatus sums the ledger rows in scope and adds one reservation for
// the request about to start plus one per unknown-cost attempt.
func budgetStatus(ctx context.Context, snap storage.Snapshot, b Budget, now time.Time) (BudgetStatus, error) {
	reserve := b.ReserveUSD
	if reserve <= 0 {
		reserve = defaultReserveUSD
	}
	status := BudgetStatus{Budget: b, ReservedUSD: reserve}
	prefix := "usage/"
	if b.Scope == "conversation" {
		prefix += b.ConversationID + "/"
	}
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return status, err
		}
		page, err := snap.Page(prefix, after, 500)
		if err != nil {
			return status, err
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			after = row.Key
			rec, _, err := read[UsageRecord](snap, row.Key)
			if err != nil {
				return status, err
			}
			if b.Window > 0 && !rec.Time.IsZero() && rec.Time.Before(now.Add(-b.Window)) {
				continue
			}
			if rec.Status == "known" {
				status.SpentUSD += rec.Usage.CostUSD
				status.Tokens += rec.Usage.InputTokens + rec.Usage.OutputTokens
			} else {
				status.Unknown++
				status.ReservedUSD += reserve
			}
		}
	}
	switch {
	case b.LimitUSD > 0 && status.SpentUSD+status.ReservedUSD > b.LimitUSD:
		status.Exceeded = fmt.Sprintf("%s budget: known %.4f USD plus reserved %.4f USD exceeds limit %.4f USD (%d attempt(s) with unknown cost)", b.Scope, status.SpentUSD, status.ReservedUSD, b.LimitUSD, status.Unknown)
	case b.LimitTokens > 0 && status.Tokens >= b.LimitTokens:
		status.Exceeded = fmt.Sprintf("%s budget: %d tokens reach limit %d", b.Scope, status.Tokens, b.LimitTokens)
	}
	return status, nil
}
