package continuous

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/provider"
)

// UsageRecord is one immutable ledger row per model attempt. Cost is known
// only when the provider reported usage for a completed attempt. Interrupted
// or failed attempts without usage are recorded as unknown, never as zero.
type UsageRecord struct {
	ConversationID string         `json:"conversation_id"`
	RunID          string         `json:"run_id"`
	Turn           int            `json:"turn"`
	Attempt        int            `json:"attempt"`
	Provider       string         `json:"provider"`
	Model          string         `json:"model"`
	Usage          provider.Usage `json:"usage"`
	// Status is known, unknown, or estimated.
	Status string `json:"status"`
	// Source is model or tool.
	Source string `json:"source"`
	Tool   string `json:"tool,omitempty"`
	// Time is the commit time of rows written since it was introduced.
	Time time.Time `json:"time,omitzero"`
}

// UsageTotals is a projection over ledger rows. Unknown counts attempts whose
// cost could not be determined; they are not included in Usage.
type UsageTotals struct {
	Usage   provider.Usage `json:"usage"`
	Known   int            `json:"known"`
	Unknown int            `json:"unknown"`
}

func usageKey(conversationID string, sequence uint64) string {
	return fmt.Sprintf("usage/%s/%020d", conversationID, sequence)
}

// Usage sums the ledger of one conversation at one committed snapshot.
func (r *Runtime) Usage(ctx context.Context, conversationID string) (UsageTotals, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return UsageTotals{}, err
	}
	return usageTotals(ctx, snap, conversationID)
}

func usageTotals(ctx context.Context, snap storage.Snapshot, conversationID string) (UsageTotals, error) {
	var totals UsageTotals
	after := ""
	prefix := "usage/" + conversationID + "/"
	for {
		if err := ctx.Err(); err != nil {
			return totals, err
		}
		page, err := snap.Page(prefix, after, 200)
		if err != nil {
			return totals, err
		}
		if len(page) == 0 {
			return totals, nil
		}
		for _, row := range page {
			after = row.Key
			var rec UsageRecord
			if err := json.Unmarshal(row.Value, &rec); err != nil {
				return totals, storage.ErrCorrupt
			}
			if rec.Status == "known" {
				totals.Usage = totals.Usage.Add(rec.Usage)
				totals.Known++
			} else {
				totals.Unknown++
			}
		}
	}
}
