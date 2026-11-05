package modes

import (
	"context"
	"strings"
	"time"
)

func (i *Interactive) runWIP(ctx context.Context, args []string) {
	if i.rejectAttachedAction("/wip", "WIP mode requires a local session") || i.agent == nil {
		return
	}
	command := "menu"
	if len(args) == 1 {
		command = strings.ToLower(args[0])
	}
	if len(args) > 1 {
		command = "invalid"
	}
	switch command {
	case "menu":
		i.openWIPDialog()
		return
	case "toggle":
		i.agent.SetWIP(!i.agent.WIPState().Active)
	case "on":
		i.agent.SetWIP(true)
	case "off":
		i.agent.SetWIP(false)
	case "done":
		i.agent.SetWIP(false)
		i.submitOrQueuePrompt(ctx, i.agent.WIPDonePrompt())
		return
	case "status":
	default:
		i.mu.Lock()
		i.statusErr = "usage: /wip [on|off|status|done]"
		i.mu.Unlock()
		return
	}
	status := i.wipStatus()
	i.mu.Lock()
	i.statusOK = status
	i.statusErr = ""
	i.mu.Unlock()
}

func (i *Interactive) wipStatus() string {
	state := i.agent.WIPState()
	if state.Active {
		return "WIP on since " + state.Since.Format(time.RFC3339)
	}
	return "WIP off"
}

func (i *Interactive) openWIPDialog() {
	choice := 1
	if i.agent.WIPState().Active {
		choice = 0
	}
	i.settingsDialog.OpenDirectOption(settingsItem{
		key:    "wip",
		label:  "wip",
		desc:   i.wipStatus(),
		choice: choice,
		options: []settingsOption{
			{value: "on", label: "on", desc: "Treat ongoing work as draft and remove abandoned approaches cleanly."},
			{value: "off", label: "off", desc: "Disable draft guidance for subsequent prompts."},
			{value: "done", label: "done", desc: "Disable WIP and submit a cleanup request."},
		},
	})
}
