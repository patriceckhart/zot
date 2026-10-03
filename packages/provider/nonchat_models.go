package provider

import (
	"sort"
	"sync"
)

// NonChatModel is a classifier or image model, kept separate from chat pickers.
// Endpoint and pricing metadata remain host-only when shown to scripts.
type NonChatModel struct {
	Model
	Type   string
	Input  []string
	Output []string
}

var nonChatMu sync.RWMutex
var userNonChatModels []NonChatModel

// NonChatModels returns registered auxiliary models. Missing prices are unknown,
// not a promise that inference is free. Reported service costs take precedence.
func NonChatModels() []NonChatModel {
	models := builtinNonChatModels()
	// Local chat models can classify by reading next-token probabilities rather
	// than by generating an answer. They keep their normal chat catalog entries.
	for _, model := range Known() {
		if model.Provider == LlamaCPPProviderID {
			model.API = "llama-cpp-classify"
			models = append(models, NonChatModel{Model: model, Type: "classifier", Input: []string{"text"}})
		}
	}
	nonChatMu.RLock()
	defer nonChatMu.RUnlock()
	index := map[string]int{}
	for i, model := range models {
		index[model.Type+"\x00"+model.Provider+"\x00"+model.ID] = i
	}
	for _, model := range userNonChatModels {
		key := model.Type + "\x00" + model.Provider + "\x00" + model.ID
		if i, ok := index[key]; ok {
			models[i] = model
		} else {
			index[key] = len(models)
			models = append(models, model)
		}
	}
	for i := range models {
		models[i].Input = append([]string(nil), models[i].Input...)
		models[i].Output = append([]string(nil), models[i].Output...)
	}
	sort.Slice(models, func(i, j int) bool {
		a, b := models[i], models[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.ID < b.ID
	})
	return models
}

func setNonChatModels(models []NonChatModel) {
	nonChatMu.Lock()
	defer nonChatMu.Unlock()
	userNonChatModels = append([]NonChatModel(nil), models...)
}
