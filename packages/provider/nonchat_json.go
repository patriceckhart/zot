package provider

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
)

// jsonObjectKeys preserves JavaScript object enumeration order, including its
// integer-index ordering. Classifier labels must not change during JSON copying.
func jsonObjectKeys(raw json.RawMessage) []string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil
	}
	var keys []string
	seen := map[string]bool{}
	for decoder.More() {
		value, err := decoder.Token()
		if err != nil {
			return nil
		}
		key, ok := value.(string)
		if !ok {
			return nil
		}
		var discard json.RawMessage
		if decoder.Decode(&discard) != nil {
			return nil
		}
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	index := func(key string) (uint64, bool) {
		value, err := strconv.ParseUint(key, 10, 32)
		return value, err == nil && value < 4294967295 && strconv.FormatUint(value, 10) == key
	}
	sort.SliceStable(keys, func(i, j int) bool {
		a, ai := index(keys[i])
		b, bi := index(keys[j])
		if ai != bi {
			return ai
		}
		return ai && a < b
	})
	return keys
}
