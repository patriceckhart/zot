package continuous

import (
	"bytes"
	"fmt"
	"html"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/patriceckhart/zot/packages/provider"
)

// MaxAttachmentBytes bounds the combined decoded file and image payload of
// one submission. Admissions store three copies in an 8 MiB commit.
const MaxAttachmentBytes = 1 << 20
const MaxAttachments = 16

// FileAttachment carries client-side bytes, never a path for the host to read
// or write. Supported images become image blocks, UTF-8 files become context.
type FileAttachment struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// PrepareSubmission validates and copies attachments, expanding text files
// into labelled context. Empty text is allowed when attachments are present.
func PrepareSubmission(text string, images []provider.ImageBlock, files []FileAttachment) (string, []provider.ImageBlock, error) {
	if len(images)+len(files) > MaxAttachments {
		return "", nil, fmt.Errorf("at most %d attachments per submission", MaxAttachments)
	}
	size := 0
	var copied []provider.ImageBlock
	for _, img := range images {
		size += len(img.Data)
		if size > MaxAttachmentBytes {
			return "", nil, fmt.Errorf("attachments exceed %d bytes", MaxAttachmentBytes)
		}
		mime := http.DetectContentType(img.Data)
		if !supportedImage(mime) || img.MimeType != mime {
			return "", nil, fmt.Errorf("image must contain PNG, JPEG, GIF, or WebP bytes matching its MIME type")
		}
		copied = append(copied, provider.ImageBlock{MimeType: mime, Data: bytes.Clone(img.Data)})
	}
	var context strings.Builder
	for _, file := range files {
		if strings.TrimSpace(file.Name) == "" {
			return "", nil, fmt.Errorf("attachment name required")
		}
		size += len(file.Data)
		if size > MaxAttachmentBytes {
			return "", nil, fmt.Errorf("attachments exceed %d bytes", MaxAttachmentBytes)
		}
		mime := http.DetectContentType(file.Data)
		fmt.Fprintf(&context, "\n\n<file name=\"%s\">\n", html.EscapeString(file.Name))
		if supportedImage(mime) {
			copied = append(copied, provider.ImageBlock{MimeType: mime, Data: bytes.Clone(file.Data)})
			context.WriteString("Attached image.")
		} else {
			if !utf8.Valid(file.Data) || bytes.IndexByte(file.Data, 0) >= 0 {
				return "", nil, fmt.Errorf("file attachments must be UTF-8 text or PNG, JPEG, GIF, or WebP images")
			}
			context.Write(file.Data)
		}
		context.WriteString("\n</file>")
	}
	return text + context.String(), copied, nil
}

func supportedImage(mime string) bool {
	return mime == "image/png" || mime == "image/jpeg" || mime == "image/gif" || mime == "image/webp"
}

func sameImages(a, b []provider.ImageBlock) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].MimeType != b[i].MimeType || !bytes.Equal(a[i].Data, b[i].Data) {
			return false
		}
	}
	return true
}

func userEntryMessage(e Entry) provider.Message {
	msg := provider.Message{Role: provider.RoleUser, Time: e.Time}
	if e.Content != "" {
		msg.Content = append(msg.Content, provider.TextBlock{Text: e.Content})
	}
	for _, img := range e.Images {
		msg.Content = append(msg.Content, img)
	}
	return msg
}
