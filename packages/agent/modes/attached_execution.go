package modes

// rejectAttachedAction refuses an action that would execute through the local
// agent instead of the prompt driver. Unsupported actions must not fall back to
// local credentials, tools, or transcript mutation.
func (i *Interactive) rejectAttachedAction(action, guidance string) bool {
	if i.cfg.PromptDriver == nil {
		return false
	}
	i.mu.Lock()
	i.statusErr = action + " is unavailable while attached: " + guidance
	i.statusOK = ""
	i.mu.Unlock()
	i.invalidate()
	return true
}
