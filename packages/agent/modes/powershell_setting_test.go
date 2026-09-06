package modes

import (
	"errors"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/tui"
)

type testPowerShellStore struct {
	SettingsStore
	values []bool
	err    error
}

func (s *testPowerShellStore) SetPowerShell(value bool) error {
	if s.err != nil {
		return s.err
	}
	s.values = append(s.values, value)
	return nil
}

func TestPowerShellSettingLiveToggle(t *testing.T) {
	cwd := t.TempDir()
	sb := tools.NewSandbox(cwd)
	store := &testPowerShellStore{}
	read := &tools.ReadTool{CWD: cwd}
	original := core.Registry{"read": read, "custom": read}
	i := &Interactive{
		cfg:   InteractiveConfig{CWD: cwd, Sandbox: sb, SettingsStore: store},
		agent: core.NewAgent(nil, "", "", original),
	}
	for _, value := range []bool{true, false} {
		i.applyPowerShellSetting(value, "windows")
		if i.statusErr != "" {
			t.Fatal(i.statusErr)
		}
		if i.cfg.PowerShellEnabled == nil || *i.cfg.PowerShellEnabled != value {
			t.Fatal("preference not updated")
		}
		if (i.agent.Tools["powershell"] != nil) != value {
			t.Fatal("live registry not updated")
		}
		if value {
			ps := i.agent.Tools["powershell"].(*tools.PowerShellTool)
			if ps.CWD != cwd || ps.Sandbox != sb {
				t.Fatal("tool lost session context")
			}
		}
		if i.agent.Tools["custom"] != read || i.agent.Tools["read"] != read {
			t.Fatal("unrelated tools changed")
		}
		if original["powershell"] != nil {
			t.Fatal("original registry mutated")
		}
	}
	if len(store.values) != 2 || !store.values[0] || store.values[1] {
		t.Fatalf("saved: %v", store.values)
	}
}

func TestPowerShellSettingFailurePreservesState(t *testing.T) {
	store := &testPowerShellStore{err: errors.New("save failed")}
	i := &Interactive{
		cfg:            InteractiveConfig{SettingsStore: store},
		agent:          core.NewAgent(nil, "", "", core.Registry{}),
		settingsDialog: newSettingsDialog(),
	}
	i.settingsDialog.Open(i.appendPowerShellSetting(nil, "windows"))
	action := i.settingsDialog.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if !action.Toggle || !action.Value {
		t.Fatal("dialog did not enable")
	}
	i.applyPowerShellSetting(action.Value, "windows")
	if i.statusErr != "settings: save failed" {
		t.Fatalf("error: %s", i.statusErr)
	}
	if i.cfg.PowerShellEnabled != nil || i.agent.Tools["powershell"] != nil || i.settingsDialog.items[0].value {
		t.Fatal("failed save changed state")
	}
}

func TestPowerShellSettingPlatformAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, goos string
		cfg        InteractiveConfig
		custom     bool
	}{
		{"non-Windows", "darwin", InteractiveConfig{}, false},
		{"no tools", "windows", InteractiveConfig{NoTools: true}, false},
		{"explicit tools", "windows", InteractiveConfig{ToolSelectionExplicit: true}, false},
		{"custom tool", "windows", InteractiveConfig{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &testPowerShellStore{}
			tc.cfg.SettingsStore = store
			reg := core.Registry{}
			if tc.custom {
				reg["powershell"] = &tools.ReadTool{}
			}
			i := &Interactive{cfg: tc.cfg, agent: core.NewAgent(nil, "", "", reg)}
			items := i.appendPowerShellSetting(nil, tc.goos)
			if tc.goos != "windows" {
				if len(items) != 0 {
					t.Fatal("setting shown on non-Windows")
				}
			} else if len(items) != 1 || !items[0].disabled || items[0].hint == "" {
				t.Fatalf("setting: %+v", items)
			}
			i.applyPowerShellSetting(true, tc.goos)
			if i.statusErr == "" || len(store.values) != 0 || i.cfg.PowerShellEnabled != nil {
				t.Fatal("override not respected")
			}
			if i.agent.Tools["powershell"] != reg["powershell"] {
				t.Fatal("registry changed")
			}
		})
	}
}

func TestPowerShellSettingReflectsPreference(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		i := &Interactive{cfg: InteractiveConfig{PowerShellEnabled: &enabled}}
		items := i.appendPowerShellSetting(nil, "windows")
		if len(items) != 1 || items[0].value != enabled || items[0].disabled {
			t.Fatalf("setting: %+v", items)
		}
	}
}
