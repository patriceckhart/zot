package modes

import (
	"context"

	"github.com/patriceckhart/zot/packages/agent/attachments"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/provider"
)

// prepareEditorPrompt transfers only explicitly selected files. Text merely
// mentioning a path is never read, directory references remain references.
func (i *Interactive) prepareEditorPrompt(ctx context.Context, text string) (string, []provider.ImageBlock, error) {
	text, images := preparePromptWithClipboardImages(text, i.clipboardImages)
	if i.cfg.PromptDriverWithImages == nil || looksLikeSlashCommand(text) {
		return expandFileChips(text, i.cfg.CWD), images, nil
	}
	if _, shell := shellEscapeCommand(text); shell {
		return expandFileChips(text, i.cfg.CWD), images, nil
	}
	paths := i.ed.SubmissionFiles()
	for _, match := range fileChipRE.FindAllStringSubmatch(text, -1) {
		if match[1] == "file" && !editorFileChipRE.MatchString(match[2]) {
			paths = append(paths, match[2])
		}
	}
	files, err := attachments.ReadFiles(ctx, i.cfg.CWD, paths, i.cfg.Sandbox)
	if err != nil {
		return "", nil, err
	}
	return continuous.PrepareSubmission(expandFileChips(text, i.cfg.CWD), images, files)
}
