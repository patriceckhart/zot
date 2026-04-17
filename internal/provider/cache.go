package provider

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// ModelCache is the on-disk shape for discovered models.
type ModelCache struct {
	FetchedAt time.Time `json:"fetched_at"`
	Models    []Model   `json:"models"`
}

// CacheTTL is how long a discovered list is considered fresh.
const CacheTTL = 6 * time.Hour

// LoadCache reads the model cache from path. Returns an empty ModelCache
// (no error) if the file does not exist.
func LoadCache(path string) (ModelCache, error) {
	var c ModelCache
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	for i := range c.Models {
		if c.Models[i].Source == "" {
			c.Models[i].Source = "cache"
		}
	}
	return c, nil
}

// SaveCache writes the cache atomically.
func SaveCache(path string, c ModelCache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// IsFresh reports whether the cache was fetched within CacheTTL.
func (c ModelCache) IsFresh() bool {
	if c.FetchedAt.IsZero() {
		return false
	}
	return time.Since(c.FetchedAt) < CacheTTL
}
