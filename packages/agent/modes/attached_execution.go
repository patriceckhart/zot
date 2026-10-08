package modes

import "github.com/patriceckhart/zot/packages/provider"

func (i *Interactive) hasPromptDriver() bool {
	return i.cfg.PromptDriver != nil || i.cfg.PromptDriverWithImages != nil
}

// rejectAttachedImagePrompt protects embedders with a legacy text-only driver.
func (i *Interactive) rejectAttachedImagePrompt(images []provider.ImageBlock) bool {
	return len(images) > 0 && i.cfg.PromptDriverWithImages == nil && i.rejectAttachedAction("image prompts", "this prompt driver supports text only")
}

// rejectAttachedAction refuses an action that would execute through the local
// agent instead of the prompt driver. Unsupported actions must not fall back to
// local credentials, tools, or transcript mutation.
func (i *Interactive) rejectAttachedAction(action, guidance string) bool {
	if !i.hasPromptDriver() {
		return false
	}
	i.mu.Lock()
	i.statusErr = action + " is unavailable while attached: " + guidance
	i.statusOK = ""
	i.mu.Unlock()
	i.invalidate()
	return true
}
