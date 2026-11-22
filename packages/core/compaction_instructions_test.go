package core

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

type compactionRequestClient struct {
	compactLifecycleClient
	request provider.Request
}

func (c *compactionRequestClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.request = req
	return c.compactLifecycleClient.Stream(ctx, req)
}

func TestCompactionInstructions(t *testing.T) {
	for _, method := range []string{"compact", "summarize"} {
		for _, template := range []string{"", " \n ", "  Write a single paragraph without headings.  "} {
			for _, custom := range []string{"", " \n ", "  Reference docs by path.  "} {
				t.Run(method+"/"+template+"/"+custom, func(t *testing.T) {
					client := &compactionRequestClient{}
					a := NewAgent(client, "test", "ordinary system instructions", nil)
					a.CompactionPrompt = template
					a.CompactionInstructions = custom
					messages := []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "work in progress"}}}}
					a.SetMessages(messages)
					extra := ""
					var err error
					if method == "compact" {
						_, err = a.Compact(context.Background(), 0, nil)
					} else {
						extra = "Focus on blockers."
						_, _, err = a.Summarize(context.Background(), messages, extra, nil)
					}
					if err != nil {
						t.Fatal(err)
					}
					wantTemplate := strings.TrimSpace(template)
					wantSystem := customSummarizationSystem
					if wantTemplate == "" {
						wantTemplate = compactionPrompt
						wantSystem = summarizationSystem
					}
					want := "<conversation>\n" + serializeTranscript(messages) + "\n</conversation>\n\n" + wantTemplate
					for _, addition := range []string{custom, extra} {
						if text := strings.TrimSpace(addition); text != "" {
							want += "\n\nAdditional instructions: " + text
						}
					}
					got := client.request.Messages[0].Content[0].(provider.TextBlock).Text
					if got != want || client.request.System != wantSystem || len(client.request.Tools) != 0 {
						t.Fatalf("unexpected summarization request: %+v", client.request)
					}
					if strings.TrimSpace(template) != "" && (strings.Contains(got, compactionPrompt) || strings.Contains(client.request.System, "structured summary")) {
						t.Fatal("custom template retained built-in format constraints")
					}
					if method == "summarize" && !reflect.DeepEqual(a.Messages(), messages) {
						t.Fatal("Summarize changed transcript")
					}
				})
			}
		}
	}
}
