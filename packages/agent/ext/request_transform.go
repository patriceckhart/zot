package ext

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

type RequestTransformEvent = extproto.RequestTransformFromHost
type RequestSegment = extproto.RequestSegment
type RequestTransformOptions = extproto.RequestTransformOptions

// RequestTransformHandler returns replacements by segment ID. Returning an error
// rejects the request under the configured failure policy. Never log the event
// by default. The context ends on timeout or extension shutdown.
type RequestTransformHandler func(context.Context, RequestTransformEvent) (map[string]json.RawMessage, error)

// InterceptBeforeRequest must be registered before Run. The capability itself
// declares the transform requirement, so a lost subscription cannot bypass it.
// Hosts without support cause Run to fail instead of silently disabling it.
func (e *Extension) InterceptBeforeRequest(options RequestTransformOptions, fn RequestTransformHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.requestTransform = fn
	e.requestOptions = options
	if fn != nil {
		found := false
		for _, cap := range e.caps {
			if cap == "request_transform" {
				found = true
			}
		}
		if !found {
			e.caps = append(e.caps, "request_transform")
		}
	}
}

func (e *Extension) dispatchRequestTransform(event RequestTransformEvent) {
	response := extproto.RequestTransformResponseFromExt{Type: "request_transform_response", ID: event.ID}
	defer func() {
		if recover() != nil {
			response.Replacements = nil
			response.Error = "request transform handler panicked"
		}
		if err := e.send(response); err != nil {
			_ = e.send(extproto.RequestTransformResponseFromExt{Type: "request_transform_response", ID: event.ID, Error: "invalid or oversized transform response"})
		}
	}()
	e.mu.Lock()
	fn, options := e.requestTransform, e.requestOptions
	e.mu.Unlock()
	if fn == nil {
		response.Error = "request transform handler unavailable"
		return
	}
	timeout := options.TimeoutMS
	if timeout == 0 {
		timeout = 30000
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Millisecond)
	if event.DeadlineMS != 0 {
		deadline = time.UnixMilli(event.DeadlineMS)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	go func() {
		select {
		case <-e.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	replacements, err := fn(ctx, event)
	if err != nil || ctx.Err() != nil {
		response.Error = "request transform rejected request"
		return
	}
	response.Replacements = replacements
}

var errRequestTransformUnsupported = errors.New("host does not support required request_transform capability")
