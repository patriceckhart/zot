package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/provider"
)

func addRequestExtension(t *testing.T, m *Manager, name string, options extproto.RequestTransformOptions, response func(extproto.RequestTransformFromHost) []byte) *Extension {
	t.Helper()
	ext := &Extension{Manifest: Manifest{Name: name}, requestTransform: true, requestHandshake: true, requestOptions: options}
	ext.stdin = startWriter{write: func(b []byte) (int, error) {
		var event extproto.RequestTransformFromHost
		if err := json.Unmarshal(b, &event); err != nil {
			return 0, err
		}
		if event.Type != "request_transform" || event.Provider != "provider" || event.SessionID != "session" || event.RequestHash == "" {
			t.Error("missing transform metadata")
		}
		if raw := response(event); raw != nil {
			m.receiveRequestTransform(ext, raw)
		}
		return len(b), nil
	}}
	m.registerRequestTransform(ext)
	return ext
}

func requestResponse(id, text string) []byte {
	raw, _ := json.Marshal(text)
	b, _ := json.Marshal(extproto.RequestTransformResponseFromExt{Type: "request_transform_response", ID: id, Replacements: map[string]json.RawMessage{"system": raw}})
	return b
}

func TestRequestTransformChainAndHash(t *testing.T) {
	m := New(t.TempDir(), "", "test", "provider", "model", nil)
	var hashes []string
	for _, name := range []string{"z-first", "a-second"} {
		addRequestExtension(t, m, name, extproto.RequestTransformOptions{}, func(ev extproto.RequestTransformFromHost) []byte {
			hashes = append(hashes, ev.RequestHash)
			return requestResponse(ev.ID, ev.Segments[0].Text+"/"+name)
		})
	}
	req := provider.Request{Model: "model", System: "base"}
	for i := 0; i < 2; i++ {
		out, err := m.TransformRequest(context.Background(), "session", "provider", "chat", req)
		if err != nil || out.System != "base/z-first/a-second" || req.System != "base" {
			t.Fatalf("chain failed: %q %v", out.System, err)
		}
	}
	if hashes[0] == hashes[1] || hashes[0] != hashes[2] || hashes[1] != hashes[3] {
		t.Fatal("hash does not reflect chained content or identical retry")
	}
}

func TestRequestTransformFailurePolicies(t *testing.T) {
	for _, policy := range []string{"fail", "pass"} {
		for _, failure := range []string{"invalid", "rejected", "timeout", "disconnected", "unknown-id", "oversized"} {
			t.Run(policy+"/"+failure, func(t *testing.T) {
				m := New(t.TempDir(), "", "test", "provider", "model", nil)
				ext := addRequestExtension(t, m, "transform", extproto.RequestTransformOptions{OnTimeout: policy, TimeoutMS: 5}, func(ev extproto.RequestTransformFromHost) []byte {
					switch failure {
					case "invalid":
						return []byte(`{"type":"request_transform_response","id":"` + ev.ID + `","replacements":{"system":42}}`)
					case "rejected":
						return []byte(`{"type":"request_transform_response","id":"` + ev.ID + `","error":"SECRET"}`)
					case "unknown-id":
						return requestResponse("not-pending", "SECRET")
					case "oversized":
						return requestResponse(ev.ID, strings.Repeat("s", (16<<20)+1))
					}
					return nil
				})
				if failure == "disconnected" {
					ext.requestClosed = true
				}
				req := provider.Request{System: "base"}
				out, err := m.TransformRequest(context.Background(), "session", "provider", "chat", req)
				if (err != nil) != (policy == "fail") || out.System != "base" {
					t.Fatalf("policy ignored: %q %v", out.System, err)
				}
				if err != nil && strings.Contains(err.Error(), "SECRET") {
					t.Fatal("error leaked extension content")
				}
				ext.mu.Lock()
				pending := len(ext.pendingTransform)
				ext.mu.Unlock()
				if pending != 0 {
					t.Fatal("pending transform leaked")
				}
			})
		}
	}
}

func TestRequestTransformStrictestPolicyAndReload(t *testing.T) {
	m := New(t.TempDir(), "", "test", "provider", "model", nil)
	addRequestExtension(t, m, "optional", extproto.RequestTransformOptions{OnTimeout: "pass"}, func(ev extproto.RequestTransformFromHost) []byte {
		return []byte(`{"type":"request_transform_response","id":"` + ev.ID + `","error":"failure"}`)
	})
	addRequestExtension(t, m, "required", extproto.RequestTransformOptions{}, func(ev extproto.RequestTransformFromHost) []byte { return requestResponse(ev.ID, "changed") })
	if _, err := m.TransformRequest(context.Background(), "session", "provider", "chat", provider.Request{}); err == nil {
		t.Fatal("strictest policy did not win")
	}
	// A failed reload or removed capability cannot silently drop requirements.
	m.Reload(context.Background(), time.Millisecond)
	if len(m.requestTransforms) != 2 {
		t.Fatal("reload removed transform registrations")
	}
	replacement := &Extension{Manifest: Manifest{Name: "required"}, requestOptions: extproto.RequestTransformOptions{OnTimeout: "pass"}}
	m.registerRequestTransform(replacement)
	if replacement.requestOptions.OnTimeout != "fail" {
		t.Fatal("reload weakened required policy")
	}
	if _, err := m.TransformRequest(context.Background(), "session", "provider", "chat", provider.Request{}); err == nil {
		t.Fatal("reload lost requirement")
	}
}

func TestRequestTransformCancellationAndBlockedPipe(t *testing.T) {
	m := New(t.TempDir(), "", "test", "provider", "model", nil)
	r, w := io.Pipe()
	defer r.Close()
	pipe := newOrderedPipe(w)
	defer pipe.Close()
	ext := &Extension{Manifest: Manifest{Name: "blocked"}, requestTransform: true, requestHandshake: true, stdin: pipe}
	m.registerRequestTransform(ext)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.TransformRequest(ctx, "session", "provider", "chat", provider.Request{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored")
	}
	ext.requestOptions.TimeoutMS = 5
	done := make(chan error, 1)
	go func() {
		_, err := m.TransformRequest(context.Background(), "session", "provider", "chat", provider.Request{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked pipe allowed request")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked write ignored deadline")
	}
}

func TestRequestTransformConcurrentCorrelation(t *testing.T) {
	m := New(t.TempDir(), "", "test", "provider", "model", nil)
	addRequestExtension(t, m, "transform", extproto.RequestTransformOptions{}, func(ev extproto.RequestTransformFromHost) []byte {
		return requestResponse(ev.ID, ev.Segments[0].Text+" clean")
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := m.TransformRequest(context.Background(), "session", "provider", "chat", provider.Request{System: "base"})
			if err != nil || out.System != "base clean" {
				t.Errorf("concurrent response misrouted: %v", err)
			}
		}()
	}
	wg.Wait()
}
