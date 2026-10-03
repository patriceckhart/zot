package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestNonChatRetriesRebuildRequestBody(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != `{"model":"synthetic"}` {
			t.Errorf("retry body changed: %q %v", data, err)
		}
		if r.Header.Get("Authorization") != "Bearer synthetic" {
			t.Error("retry lost authorization")
		}
		if calls.Add(1) == 1 {
			w.Header().Set("retry-after-ms", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()
	result, err := (NonChatClient{HTTP: server.Client()}).post(context.Background(), server.URL, "synthetic", map[string]string{"model": "synthetic"})
	if err != nil || calls.Load() != 2 || string(result["ok"]) != "true" {
		t.Fatalf("calls=%d result=%+v error=%v", calls.Load(), result, err)
	}
}
