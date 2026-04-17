// Package modes implements zot's three run modes: print, json, interactive.
package modes

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
)

// RunPrint runs the agent to completion and writes only the final
// assistant text block to out. Exit code comes from the caller.
func RunPrint(ctx context.Context, ag *core.Agent, prompt string, images []provider.ImageBlock, out io.Writer) error {
	var finalText strings.Builder
	var lastAssistant string
	var runErr error

	sink := func(ev core.AgentEvent) {
		switch e := ev.(type) {
		case core.EvAssistantMessage:
			// Keep the most recent assistant text block; by the end it's the final answer.
			var sb strings.Builder
			for _, c := range e.Message.Content {
				if tb, ok := c.(provider.TextBlock); ok {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(tb.Text)
				}
			}
			if sb.Len() > 0 {
				lastAssistant = sb.String()
			}
		case core.EvTurnEnd:
			if e.Err != nil {
				runErr = e.Err
			}
		}
	}

	if err := ag.Prompt(ctx, prompt, images, sink); err != nil {
		return err
	}
	if runErr != nil {
		return runErr
	}

	finalText.WriteString(lastAssistant)
	if finalText.Len() > 0 && !strings.HasSuffix(finalText.String(), "\n") {
		finalText.WriteString("\n")
	}
	_, err := fmt.Fprint(out, finalText.String())
	return err
}
