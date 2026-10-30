package provider

// appendCatalog adds supplements without duplicating provider/model identities.
// Existing entries win, matching FindModel's first-match lookup and preserving
// curated metadata when a bulk catalog also includes the same model.
func appendCatalog(base, extra []Model) []Model {
	out := make([]Model, 0, len(base)+len(extra))
	seen := make(map[[2]string]bool, len(base)+len(extra))
	for _, group := range [][]Model{base, extra} {
		for _, model := range group {
			key := [2]string{model.Provider, model.ID}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, model)
		}
	}
	return out
}

// MergeCatalog returns the baked-in catalog overlaid with live entries.
// Precedence per id: live > catalog; speculative entries are promoted
// to non-speculative when a matching live id appears.
//
// Unknown live ids (not in the static catalog) are appended at the end
// with placeholder prices so they still render in the picker. Prices
// can be populated later from a richer catalog source.
func MergeCatalog(live []Model) []Model {
	byKey := func(p, id string) string { return p + "/" + id }

	staticIndex := make(map[string]Model, len(Catalog))
	staticOrder := make([]string, 0, len(Catalog))
	for _, m := range Catalog {
		m.Source = "catalog"
		k := byKey(m.Provider, m.ID)
		staticIndex[k] = m
		staticOrder = append(staticOrder, k)
	}

	seenLive := make(map[string]bool, len(live))
	for _, m := range live {
		seenLive[byKey(m.Provider, m.ID)] = true
	}

	// Promote/overwrite from live.
	for _, l := range live {
		k := byKey(l.Provider, l.ID)
		if s, ok := staticIndex[k]; ok {
			// These gateways report authoritative metadata. Their seed
			// entries are only offline fallbacks.
			if l.Provider == "yolo-auto" || l.Provider == "cheaperinference" || l.Provider == "opper" || l.Provider == "runcrate" || l.Provider == "cloudgpu" {
				staticIndex[k] = l
				continue
			}
			// Keep static prices & context window (live endpoint rarely
			// exposes these), but mark as live and non-speculative.
			s.Source = "live"
			s.Speculative = false
			if l.DisplayName != "" {
				s.DisplayName = l.DisplayName
			}
			staticIndex[k] = s
		} else {
			// New live id we'd never heard of. Best-effort defaults.
			if l.DisplayName == "" {
				l.DisplayName = l.ID
			}
			staticIndex[k] = l
			staticOrder = append(staticOrder, k)
		}
	}

	out := make([]Model, 0, len(staticOrder))
	for _, k := range staticOrder {
		out = append(out, staticIndex[k])
	}
	return out
}
