package modes

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/patriceckhart/zot/packages/agent/attachments"
	"github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

var contentChipRE = regexp.MustCompile(`\[content:([^\]]+)\]`)

// selectedFilePaths includes ordinary picker and drop selections only when
// transferring to a host. Explicit content selections are handled separately.
func selectedFilePaths(ed *tui.Editor, text string, transfer bool) []string {
	if !transfer {
		return nil
	}
	paths := ed.SubmissionFiles()
	for _, match := range fileChipRE.FindAllStringSubmatch(text, -1) {
		if match[1] == "file" && !editorFileChipRE.MatchString(match[2]) {
			paths = append(paths, match[2])
		}
	}
	return paths
}

// prepareFilePrompt snapshots explicit content selections before queueing.
// Text merely mentioning a path is never read. Additional paths are explicit
// selections to transfer to a host, where local path references would not work.
func prepareFilePrompt(ctx context.Context, cwd, text string, images []provider.ImageBlock, paths []string, sandbox *tools.Sandbox) (string, []provider.ImageBlock, error) {
	for _, match := range contentChipRE.FindAllStringSubmatch(text, -1) {
		paths = append(paths, match[1])
	}
	if strings.HasPrefix(strings.TrimSpace(text), "!@") {
		return "", nil, fmt.Errorf("select a file from the !@ picker before submitting")
	}
	text = contentChipRE.ReplaceAllStringFunc(text, func(chip string) string {
		return filepath.Join(cwd, contentChipRE.FindStringSubmatch(chip)[1])
	})
	text = expandFileChips(text, cwd)
	if len(paths) == 0 && len(images) == 0 {
		return text, images, nil
	}
	files, err := attachments.ReadFiles(ctx, cwd, paths, sandbox)
	if err != nil {
		return "", nil, err
	}
	return continuous.PrepareSubmission(text, images, files)
}

func (i *Interactive) prepareEditorPrompt(ctx context.Context, text string) (string, []provider.ImageBlock, error) {
	text, images := preparePromptWithClipboardImages(text, i.clipboardImages)
	if len(images) > 0 && i.hasPromptDriver() && i.cfg.PromptDriverWithImages == nil {
		return "", nil, fmt.Errorf("image prompts are unavailable while attached, this prompt driver supports text only")
	}
	_, shell := shellEscapeCommand(text)
	if looksLikeSlashCommand(text) || shell {
		if contentChipRE.MatchString(text) {
			return "", nil, fmt.Errorf("content selections are only supported in chat prompts")
		}
		return expandFileChips(text, i.cfg.CWD), images, nil
	}
	paths := selectedFilePaths(i.ed, text, i.cfg.PromptDriverWithImages != nil)
	return prepareFilePrompt(ctx, i.cfg.CWD, text, images, paths, i.cfg.Sandbox)
}
