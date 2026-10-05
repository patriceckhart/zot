package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Outbox: durable notifications for events humans must see (pending
// approvals, blocked recovery, blocked cleanup). A notification is committed
// with the state change that needs attention, delivered by a Notifier after
// the commit, and acknowledged with a second commit. Delivery is at least
// once: a crash between delivery and acknowledgement redelivers, which is
// why the notification carries a stable ID for the integration to
// deduplicate on.

// Notification is one outbox row.
type Notification struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Subject        string          `json:"subject"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Created        time.Time       `json:"created"`
	Attempts       int             `json:"attempts"`
	LastError      string          `json:"last_error,omitempty"`
	NextAttempt    time.Time       `json:"next_attempt,omitzero"`
}

// Notifier delivers a notification. Returning an error schedules a retry
// with backoff; the notification is acknowledged only after a nil return.
// Implementations should deduplicate on Notification.ID.
type Notifier func(ctx context.Context, n Notification) error

func outboxKey(id string) string { return "outbox/" + id }

// outboxOp builds the record for a notification committed alongside the
// state it announces.
func outboxOp(id, kind, conversationID, subject string, payload any) storage.Operation {
	raw, _ := json.Marshal(payload)
	return record(outboxKey(id), Notification{ID: id, Kind: kind, ConversationID: conversationID, Subject: subject, Payload: raw, Created: time.Now().UTC()})
}

// Outbox lists undelivered notifications, oldest first.
func (r *Runtime) Outbox(ctx context.Context) ([]Notification, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := snap.Page("outbox/", "", 1000)
	if err != nil {
		return nil, err
	}
	out := make([]Notification, 0, len(rows))
	for _, row := range rows {
		var n Notification
		if json.Unmarshal(row.Value, &n) != nil {
			return nil, storage.ErrCorrupt
		}
		out = append(out, n)
	}
	return out, nil
}

// Deliver attempts every due notification once through notifier and returns
// the number acknowledged. Failures record the error and back off (1s, 2s,
// 4s... capped at one hour). It never blocks on a notifier longer than ctx.
func (r *Runtime) Deliver(ctx context.Context, notifier Notifier) (int, error) {
	if notifier == nil {
		return 0, nil
	}
	pending, err := r.Outbox(ctx)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	delivered := 0
	for _, n := range pending {
		if !n.NextAttempt.IsZero() && now.Before(n.NextAttempt) {
			continue
		}
		deliverErr := notifier(ctx, n)
		if ctx.Err() != nil {
			return delivered, ctx.Err()
		}
		for {
			snap, err := r.store.Snapshot(ctx)
			if err != nil {
				return delivered, err
			}
			current, ok, err := read[Notification](snap, outboxKey(n.ID))
			if err != nil {
				return delivered, err
			}
			if !ok {
				break
			}
			var op storage.Operation
			if deliverErr == nil {
				op = storage.Operation{Key: outboxKey(n.ID), Delete: true}
			} else {
				current.Attempts++
				current.LastError = deliverErr.Error()
				delay := time.Second << min(current.Attempts-1, 12)
				if delay > time.Hour {
					delay = time.Hour
				}
				current.NextAttempt = time.Now().UTC().Add(delay)
				op = record(outboxKey(n.ID), current)
			}
			err = r.commit(ctx, snap, "outbox.deliver", op)
			if errors.Is(err, storage.ErrConflict) {
				continue
			}
			if err != nil {
				return delivered, err
			}
			if deliverErr == nil {
				delivered++
			}
			break
		}
	}
	return delivered, nil
}

// Acknowledge removes a notification an external integration delivered
// itself. Unknown IDs are not an error: the host may have delivered it.
func (r *Runtime) Acknowledge(ctx context.Context, id string) (bool, error) {
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return false, err
		}
		if _, ok := snap.Get(outboxKey(id)); !ok {
			return false, nil
		}
		err = r.commit(ctx, snap, "outbox.ack", storage.Operation{Key: outboxKey(id), Delete: true})
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return err == nil, err
	}
}

// NextDelivery reports the earliest scheduled retry, zero when the outbox is
// empty or has only due notifications.
func (r *Runtime) NextDelivery(ctx context.Context) (time.Time, error) {
	pending, err := r.Outbox(ctx)
	if err != nil {
		return time.Time{}, err
	}
	var next time.Time
	for _, n := range pending {
		if n.NextAttempt.IsZero() {
			return time.Time{}, nil
		}
		if next.IsZero() || n.NextAttempt.Before(next) {
			next = n.NextAttempt
		}
	}
	return next, nil
}

func approvalNotificationID(approvalID string) string { return fmt.Sprintf("approval-%s", approvalID) }
