package codemode

import "fmt"

// Settings controls activation and model-facing tool presentation.
type Settings struct {
	Enabled      bool   `json:"enabled,omitempty"`
	Mode         string `json:"mode,omitempty"`
	InlineBudget *int   `json:"inlineBudget,omitempty"`
}

func (s *Settings) Validate() error {
	if s == nil {
		return nil
	}
	if s.Mode != "" && s.Mode != "on" && s.Mode != "only" {
		return fmt.Errorf("codemode.mode must be on or only")
	}
	if s.InlineBudget != nil && *s.InlineBudget < 0 {
		return fmt.Errorf("codemode.inlineBudget must be non-negative")
	}
	return nil
}
