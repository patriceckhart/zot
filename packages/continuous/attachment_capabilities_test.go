package continuous

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestAttachedImagesFailBeforeSubmissionOnOlderHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	requests := make(chan string, 8)
	go func() {
		reader := bufio.NewReader(b)
		enc := json.NewEncoder(b)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var req hostRequest
			if json.Unmarshal(line, &req) != nil {
				return
			}
			requests <- req.Method
			enc.Encode(hostResponse{ID: req.ID, Type: "response", Success: true, Data: map[string]any{}})
		}
	}()
	client, err := NewClient(ctx, a, "")
	if err != nil {
		t.Fatal(err)
	}
	view := core.NewAgent(nil, "test", "", nil)
	d := &AttachedDriver{Client: client, ConversationID: "w"}
	if err := d.PromptWithImages(ctx, view, "review", []provider.ImageBlock{attachmentImage(t)}, func(core.AgentEvent) {}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("old host: %v", err)
	}
	if method := <-requests; method != "runtime.status" {
		t.Fatalf("unexpected request: %s", method)
	}
	if len(requests) != 0 || len(view.Messages()) != 0 {
		t.Fatal("unsupported transfer admitted work or changed history")
	}
}
