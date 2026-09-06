package modes

import (
	"fmt"

	"github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/core"
)

// Optional so existing SettingsStore implementations remain compatible.
type powerShellSettingsStore interface {
	SetPowerShell(enabled bool) error
}

func (i *Interactive) powerShellSettingHint() string {
	switch {
	case i.cfg.NoTools:
		return "disabled for this run by --no-tools"
	case i.cfg.ToolSelectionExplicit:
		return "controlled for this run by --tools"
	}
	if i.agent != nil {
		if tool := i.agent.Tools["powershell"]; tool != nil {
			if _, builtin := tool.(*tools.PowerShellTool); !builtin {
				return "controlled by a custom tool"
			}
		}
	}
	return ""
}

func (i *Interactive) appendPowerShellSetting(items []settingsItem, goos string) []settingsItem {
	if goos != "windows" {
		return items
	}
	hint := i.powerShellSettingHint()
	value := i.cfg.PowerShellEnabled != nil && *i.cfg.PowerShellEnabled
	if hint != "" && i.agent != nil {
		value = i.agent.Tools["powershell"] != nil
	}
	return append(items, settingsItem{
		key: "powershell_enabled", label: "PowerShell tool",
		desc:  "enable native PowerShell alongside the default tools; execution remains blocked while jailed",
		value: value, disabled: hint != "", hint: hint,
	})
}

func (i *Interactive) applyPowerShellSetting(value bool, goos string) {
	var err error
	switch {
	case goos != "windows":
		err = fmt.Errorf("PowerShell is only available on Windows")
	case i.powerShellSettingHint() != "":
		err = fmt.Errorf("%s", i.powerShellSettingHint())
	default:
		if i.cfg.SettingsStore != nil {
			if store, ok := i.cfg.SettingsStore.(powerShellSettingsStore); ok {
				err = store.SetPowerShell(value)
			} else {
				err = fmt.Errorf("settings store does not support PowerShell")
			}
		}
	}
	if err != nil {
		i.mu.Lock()
		i.statusErr = "settings: " + err.Error()
		i.statusOK = ""
		i.mu.Unlock()
		// Restore the row that the dialog optimistically toggled.
		if i.settingsDialog != nil {
			for idx := range i.settingsDialog.items {
				if i.settingsDialog.items[idx].key == "powershell_enabled" {
					i.settingsDialog.items[idx].value = i.cfg.PowerShellEnabled != nil && *i.cfg.PowerShellEnabled
				}
			}
		}
		return
	}

	i.cfg.PowerShellEnabled = &value
	if i.agent != nil {
		next := core.Registry{}
		for name, tool := range i.agent.Tools {
			if name != "powershell" {
				next[name] = tool
			}
		}
		if value {
			next["powershell"] = &tools.PowerShellTool{CWD: i.cfg.CWD, Sandbox: i.cfg.Sandbox}
		}
		i.agent.SetTools(next)
	}
	i.mu.Lock()
	i.statusOK = "PowerShell tool " + onOff(value)
	i.statusErr = ""
	i.mu.Unlock()
}
