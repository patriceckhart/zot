package continuous

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// A budget is checked before every request. Known spend plus one reservation
// must fit; unknown-cost attempts count as reservations, never as zero. An
// exceeded budget fails the run before anything is sent.
func TestBudgetRefusesRequestConservatively(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{text: "one"},
		{err: errors.New("HTTP 503 service unavailable")},
		{text: "two"},
		{text: "never"},
	})
	defer h.r.Close()
	c := h.root(t)
	// Scripted usage reports 10 input + 5 output tokens and no cost: cap by tokens.
	if _, err := h.r.SetBudget(ctx, Budget{Scope: "conversation", ConversationID: c.ID, LimitTokens: 30}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.SetBudget(ctx, Budget{Scope: "conversation", ConversationID: c.ID, LimitTokens: 30}, 0); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("create over existing budget: %v", err)
	}
	if _, err := h.r.SetBudget(ctx, Budget{Scope: "bogus"}, 0); err == nil {
		t.Fatal("bad scope accepted")
	}
	h.r.Submit(ctx, c.ID, "a", "", "first")
	if run, _, err := h.svc.Step(ctx, c.ID); err != nil || run.Outcome != "completed" {
		t.Fatalf("first run: %+v %v", run, err)
	}
	// Second run: the failed attempt has unknown cost, the retry succeeds.
	// Tokens: 15 + 15 = 30, which reaches the limit.
	h.r.Submit(ctx, c.ID, "a", "", "second")
	if run, _, err := h.svc.Step(ctx, c.ID); err != nil || run.Outcome != "completed" {
		t.Fatalf("second run: %+v %v", run, err)
	}
	statuses, err := h.r.CheckBudgets(ctx, c.ID)
	if !errors.Is(err, ErrBudgetExceeded) || len(statuses) != 1 || statuses[0].Tokens != 30 || statuses[0].Unknown != 1 {
		t.Fatalf("status: %+v %v", statuses, err)
	}
	// Third run is refused before any request; the submission fails with the reason.
	sub, _ := h.r.Submit(ctx, c.ID, "a", "", "third")
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "failed" || !strings.Contains(run.Error, "tokens reach limit") {
		t.Fatalf("budget run: %+v %v", run, err)
	}
	if h.client.requestCount() != 3 {
		t.Fatalf("request sent despite budget: %d", h.client.requestCount())
	}
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "failed" {
		t.Fatalf("submission: %+v", s)
	}
	// Cost budgets: unknown attempts are charged the reservation.
	b, _, _ := h.r.Budget(ctx, "conversation", c.ID)
	if _, err := h.r.SetBudget(ctx, Budget{Scope: "conversation", ConversationID: c.ID, LimitUSD: 0.10, ReserveUSD: 0.04}, b.Revision); err != nil {
		t.Fatal(err)
	}
	// Known cost 0, one unknown attempt (0.04) plus the next reservation (0.04) = 0.08 <= 0.10: allowed.
	if _, err := h.r.CheckBudgets(ctx, c.ID); err != nil {
		t.Fatalf("cost budget within limit: %v", err)
	}
	b, _, _ = h.r.Budget(ctx, "conversation", c.ID)
	if _, err := h.r.SetBudget(ctx, Budget{Scope: "conversation", ConversationID: c.ID, LimitUSD: 0.07, ReserveUSD: 0.04}, b.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.CheckBudgets(ctx, c.ID); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("unknown cost counted as zero: %v", err)
	}
	// Runtime scope applies to every conversation; removing restores service.
	b, _, _ = h.r.Budget(ctx, "conversation", c.ID)
	if _, err := h.r.SetBudget(ctx, Budget{Scope: "conversation", ConversationID: c.ID}, b.Revision); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.r.Budget(ctx, "conversation", c.ID); ok {
		t.Fatal("budget not removed")
	}
	if _, err := h.r.SetBudget(ctx, Budget{Scope: "runtime", LimitTokens: 1}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.CheckBudgets(ctx, c.ID); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("runtime budget ignored: %v", err)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}
