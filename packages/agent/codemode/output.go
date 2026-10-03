package codemode

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	executor "github.com/patriceckhart/zot/packages/codemode"
	"github.com/patriceckhart/zot/packages/provider"
)

type outputCollector struct {
	limit      int64
	count      int64
	lines      int64
	texts      int
	items      []provider.Content
	head, tail string
	file       *os.File
	path       string
	fileError  error
	truncated  bool
}

func charCount(text string) int64 {
	var count int64
	for _, r := range text {
		count++
		if r > 0xffff {
			count++
		}
	}
	return count
}

func prefixChars(text string, limit int64) string {
	var count int64
	for index, r := range text {
		count++
		if r > 0xffff {
			count++
		}
		if count > limit {
			return text[:index]
		}
	}
	return text
}

func suffixChars(text string, limit int64) string {
	runes := []rune(text)
	var count int64
	for i := len(runes) - 1; i >= 0; i-- {
		count++
		if runes[i] > 0xffff {
			count++
		}
		if count > limit {
			return string(runes[i+1:])
		}
	}
	return text
}

func (o *outputCollector) add(item executor.Item) {
	if item.Type == "image" {
		data, err := base64.StdEncoding.DecodeString(item.Data)
		if err != nil {
			o.fileError = fmt.Errorf("invalid image from worker")
			return
		}
		o.items = append(o.items, provider.ImageBlock{MimeType: item.MimeType, Data: data})
		return
	}
	part := item.Text
	if o.texts > 0 {
		part = "\n" + part
	}
	o.texts++
	o.lines += int64(strings.Count(part, "\n"))
	o.count += charCount(part)
	if !o.truncated && o.count > o.limit {
		o.truncated = true
		var previous []string
		var images []provider.Content
		for _, content := range o.items {
			if text, ok := content.(provider.TextBlock); ok {
				previous = append(previous, text.Text)
			} else {
				images = append(images, content)
			}
		}
		o.file, o.fileError = os.CreateTemp("", "zot-codemode-*.log")
		if o.file != nil {
			o.path = o.file.Name()
			_, o.fileError = o.file.WriteString(strings.Join(previous, "\n"))
		}
		o.items = images
	}
	if o.truncated {
		if o.file != nil && o.fileError == nil {
			_, o.fileError = o.file.WriteString(part)
		}
	} else {
		o.items = append(o.items, provider.TextBlock{Text: item.Text})
	}
	headChars := o.limit / 2
	tailChars := o.limit - headChars
	if charCount(o.head) < headChars {
		o.head = prefixChars(o.head+part, headChars)
	}
	o.tail = suffixChars(o.tail+part, tailChars)
}

func (o *outputCollector) finish() ([]provider.Content, string) {
	if o.file != nil {
		if err := o.file.Close(); o.fileError == nil {
			o.fileError = err
		}
	}
	if !o.truncated {
		return o.items, ""
	}
	removed := o.count - charCount(o.head) - charCount(o.tail)
	text := fmt.Sprintf("Warning: truncated output (original token count: %d)\nTotal output lines: %d\n\n%s…%d tokens truncated…%s", (o.count+3)/4, o.lines+1, o.head, (removed+3)/4, o.tail)
	if o.fileError == nil {
		text += "\n\n[Full output: " + o.path + " (read with offset/limit)]"
	} else {
		text += "\n\n[Could not save the full output: " + o.fileError.Error() + "]"
		if o.path != "" {
			_ = os.Remove(o.path)
		}
		o.path = ""
	}
	return append([]provider.Content{provider.TextBlock{Text: text}}, o.items...), o.path
}
