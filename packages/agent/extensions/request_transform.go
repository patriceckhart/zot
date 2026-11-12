package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func (m *Manager) registerRequestTransform(ext *Extension) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, previous := range m.requestTransforms {
		if previous.Manifest.Name == ext.Manifest.Name {
			if previous != ext {
				previous.mu.Lock()
				strict := previous.requestOptions.OnTimeout != "pass"
				previous.mu.Unlock()
				if strict {
					ext.mu.Lock()
					ext.requestOptions.OnTimeout = "fail"
					ext.mu.Unlock()
				}
			}
			m.requestTransforms[i] = ext
			return
		}
	}
	m.requestTransforms = append(m.requestTransforms, ext)
}

// TransformRequest chains transforms in successful load order. Requirements survive
// process death and reload failure. A new manager is needed to remove a required
// transform. Extension error text is never exposed because it may contain input.
func (m *Manager) TransformRequest(ctx context.Context, sessionID, providerID, purpose string, req provider.Request) (provider.Request, error) {
	if err := ctx.Err(); err != nil {
		return req, err
	}
	m.mu.RLock()
	registered := make(map[*Extension]bool, len(m.requestTransforms))
	for _, ext := range m.requestTransforms {
		registered[ext] = true
	}
	var exts []*Extension
	for _, ext := range m.extOrder {
		if registered[ext] {
			exts = append(exts, ext)
			delete(registered, ext)
		}
	}
	// Keep failed startups and requirements from removed or failed reloads.
	for _, ext := range m.requestTransforms {
		if registered[ext] {
			exts = append(exts, ext)
		}
	}
	m.mu.RUnlock()
	strict := false
	for _, ext := range exts {
		ext.mu.Lock()
		strict = strict || ext.requestOptions.OnTimeout != "pass"
		ext.mu.Unlock()
	}
	current := req
	for _, ext := range exts {
		if err := ctx.Err(); err != nil {
			return req, err
		}
		ext.mu.Lock()
		options := ext.requestOptions
		ext.mu.Unlock()
		if options.OnTimeout != "" && options.OnTimeout != "fail" && options.OnTimeout != "pass" {
			return req, errors.New("invalid request transform failure policy")
		}
		timeout := options.TimeoutMS
		if timeout == 0 {
			timeout = 30000
		}
		if timeout < 1 || timeout > 300000 {
			return req, errors.New("invalid request transform timeout")
		}
		candidate := current
		callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
		segments, err := core.RequestSegments(current)
		if err == nil {
			wireSegments := make([]extproto.RequestSegment, len(segments))
			for i, s := range segments {
				wireSegments[i] = extproto.RequestSegment(s)
			}
			var response extproto.RequestTransformResponseFromExt
			response, err = m.askRequestTransform(callCtx, ext, extproto.RequestTransformFromHost{
				Type: "request_transform", ID: newCorrelationID(), SessionID: sessionID,
				Provider: providerID, Model: current.Model, Purpose: purpose,
				RequestHash: core.RequestTransformHash(providerID, current.Model, purpose, segments), Segments: wireSegments,
			})
			if err == nil {
				var next provider.Request
				next, err = core.ReplaceRequestSegments(current, response.Replacements)
				if err == nil {
					candidate = next
				}
			}
		}
		if callCtx.Err() != nil {
			err = callCtx.Err()
		}
		cancel()
		if ctx.Err() != nil {
			return req, ctx.Err()
		}
		if err != nil {
			// Do not include extension-provided errors, payloads or decoder errors.
			message := "before_request: " + err.Error()
			ext.recordDiagnostic(message)
			if strict {
				return req, fmt.Errorf("extension %q request transform failed: %w", ext.Manifest.Name, err)
			}
		} else {
			current = candidate
		}
	}
	return current, nil
}

func (m *Manager) askRequestTransform(ctx context.Context, ext *Extension, payload extproto.RequestTransformFromHost) (extproto.RequestTransformResponseFromExt, error) {
	var zero extproto.RequestTransformResponseFromExt
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		payload.DeadlineMS = deadline.UnixMilli()
	}
	frame, err := extproto.Encode(payload)
	if err != nil || len(frame) >= extproto.RequestTransformFrameLimit {
		return zero, errors.New("request frame limit exceeded")
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	ch := make(chan extproto.RequestTransformResponseFromExt, 1)
	ext.mu.Lock()
	if !ext.requestTransform || !ext.requestHandshake || ext.requestClosed || ext.stdin == nil {
		ext.mu.Unlock()
		return zero, errors.New("request transform unavailable")
	}
	if ext.pendingTransform == nil {
		ext.pendingTransform = make(map[string]chan extproto.RequestTransformResponseFromExt)
	}
	ext.pendingTransform[payload.ID] = ch
	stdin := ext.stdin
	ext.mu.Unlock()
	defer func() {
		ext.mu.Lock()
		delete(ext.pendingTransform, payload.ID)
		ext.mu.Unlock()
	}()
	// Include serialization and ordered pipe writes in the same deadline. A
	// closed pipe releases blocked writes and makes future calls fail closed.
	done := make(chan error, 1)
	go func() {
		var err error
		if pipe, ok := stdin.(*orderedPipe); ok {
			_, err = pipe.writeContext(ctx, frame, 5*time.Minute)
		} else {
			_, err = stdin.Write(frame)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return zero, errors.New("request write failed")
		}
	case <-ctx.Done():
		_ = stdin.Close()
		<-done
		return zero, ctx.Err()
	}
	select {
	case response := <-ch:
		if response.Error != "" {
			return zero, errors.New("extension rejected request")
		}
		return response, nil
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

func (m *Manager) receiveRequestTransform(ext *Extension, line []byte) {
	var response extproto.RequestTransformResponseFromExt
	if err := json.Unmarshal(line, &response); err != nil {
		var frame extproto.Frame
		if json.Unmarshal(line, &frame) != nil {
			return
		}
		response = extproto.RequestTransformResponseFromExt{ID: frame.ID, Error: "invalid response"}
	}
	ext.mu.Lock()
	ch := ext.pendingTransform[response.ID]
	delete(ext.pendingTransform, response.ID)
	ext.mu.Unlock()
	if ch != nil {
		ch <- response
	}
}
