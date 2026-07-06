package tools

import (
	"context"
	"encoding/json"
	"testing"
)

type stubSender struct {
	active bool
	images []string
	docs   []string
}

func (s *stubSender) SendImage(_ context.Context, path, _ string) error {
	s.images = append(s.images, path)
	return nil
}
func (s *stubSender) SendDocument(_ context.Context, path, _ string) error {
	s.docs = append(s.docs, path)
	return nil
}
func (s *stubSender) Active() bool { return s.active }

func TestMatrixSendImageInactiveBridge(t *testing.T) {
	tool := &MatrixSendImageTool{Sender: &stubSender{active: false}}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"x.png"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("inactive bridge must produce a model-visible error result")
	}
}

func TestMatrixToolNames(t *testing.T) {
	if (&MatrixSendImageTool{}).Name() != "matrix_send_image" {
		t.Fatal("image tool name")
	}
	if (&MatrixSendFileTool{}).Name() != "matrix_send_file" {
		t.Fatal("file tool name")
	}
}

// TelegramSender must remain a valid name for MediaSender (back-compat).
var _ TelegramSender = (*stubSender)(nil)
var _ MediaSender = (*stubSender)(nil)
