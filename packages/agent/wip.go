package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/core"
)

func loadWIPSettings(fallback core.WIPSettings) (core.WIPSettings, error) {
	data, err := os.ReadFile(filepath.Join(ZotHome(), "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return fallback, fallback.Validate()
	}
	if err != nil {
		return fallback, fmt.Errorf("read WIP settings: %w", err)
	}
	var settings struct {
		WIP *core.WIPSettings `json:"wip"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return fallback, fmt.Errorf("parse WIP settings: %w", err)
	}
	if settings.WIP != nil {
		fallback = *settings.WIP
	}
	return fallback, fallback.Validate()
}

func gitWIPBaseline(cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "HEAD")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	hash := strings.TrimSpace(string(out))
	if len(hash) != 40 && len(hash) != 64 {
		return ""
	}
	for _, c := range hash {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return ""
		}
	}
	return hash
}

func configureWIP(a *core.Agent, r Resolved) {
	a.WIPSettings = r.WIP
	a.WIPBaseline = func() string { return gitWIPBaseline(r.CWD) }
	a.SetWIP(r.WIP.DefaultOn || r.WIPOn)
}
