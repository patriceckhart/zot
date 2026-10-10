package modes

import (
	"bytes"
	"fmt"

	"github.com/patriceckhart/zot/packages/provider"
)

type queuedPrompt struct {
	Text   string
	Images []provider.ImageBlock
}

func newQueuedPrompt(text string, images []provider.ImageBlock) queuedPrompt {
	q := queuedPrompt{Text: text}
	for _, img := range images {
		q.Images = append(q.Images, provider.ImageBlock{MimeType: img.MimeType, Data: bytes.Clone(img.Data)})
	}
	return q
}

// queueFollowUp keeps local text at agent-loop boundaries until an image
// prompt needs the follow-up queue. Later prompts use that same queue so they
// cannot overtake the image. Image prompts run after the current turn ends.
func (i *Interactive) queueFollowUp(text string, images []provider.ImageBlock) {
	i.mu.Lock()
	ag := i.agent
	if i.hasPromptDriver() || i.compacting || ag == nil || len(images) > 0 || len(i.queued) > 0 {
		i.queued = append(i.queued, newQueuedPrompt(text, images))
		i.mu.Unlock()
		return
	}
	i.mu.Unlock()
	ag.QueueMessage(text)
}

func queuedPromptLabels(queue []queuedPrompt) []string {
	var labels []string
	for _, q := range queue {
		label := q.Text
		if len(q.Images) > 0 {
			label += fmt.Sprintf(" [%d image(s)]", len(q.Images))
		}
		labels = append(labels, label)
	}
	return labels
}

func (i *Interactive) restoreQueuedPrompt(q queuedPrompt) {
	i.clipboardImages = nil
	text := q.Text
	for n, img := range q.Images {
		marker := fmt.Sprintf("[clipboard image #%d]", n+1)
		i.clipboardImages = append(i.clipboardImages, clipboardImageAttachment{Marker: marker, Image: img})
		text += " " + marker
	}
	i.ed.SetValue(text)
}
