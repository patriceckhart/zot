package core

import (
	"encoding/json"
	"os"
)

// SessionName returns an explicitly assigned title or alias, never a prompt-derived
// display title. Legacy top-level rename rows are supported. Legacy branch
// rename rows are omitted because they may contain generated prompt excerpts.
func SessionName(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var name string
	var branch bool
	err = forEachJSONLLine(f, func(line []byte) error {
		var head sessionLineHead
		if json.Unmarshal(line, &head) != nil {
			return nil
		}
		switch head.Type {
		case "meta":
			var row struct {
				Meta SessionMeta `json:"meta"`
			}
			if json.Unmarshal(line, &row) == nil {
				branch = row.Meta.Parent != ""
				name = row.Meta.Title
				if name == "" {
					name = row.Meta.Alias
				}
			}
		case "rename":
			var row struct {
				Title    string `json:"title"`
				Explicit bool   `json:"explicit"`
			}
			if json.Unmarshal(line, &row) == nil && row.Title != "" && (!branch || row.Explicit) {
				name = row.Title
			}
		}
		return nil
	})
	return name, err
}
