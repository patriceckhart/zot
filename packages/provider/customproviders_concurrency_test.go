package provider

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func preserveUserModelRegistries(t *testing.T) {
	t.Helper()
	previous := CustomProviders()
	nonChatMu.RLock()
	previousNonChat := append([]NonChatModel(nil), userNonChatModels...)
	nonChatMu.RUnlock()
	t.Cleanup(func() {
		customProvidersMu.Lock()
		customProviders = previous
		customProvidersMu.Unlock()
		setNonChatModels(previousNonChat)
	})
}

func TestCustomProvidersReturnsIndependentSnapshot(t *testing.T) {
	preserveUserModelRegistries(t)
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{"providers":{"snapshot":{"baseUrl":"https://example.invalid/v1","auth":"none"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	LoadUserModels(path)
	snapshot := CustomProviders()
	delete(snapshot, "snapshot")
	snapshot["injected"] = CustomProviderConfig{BaseURL: "https://injected.invalid/v1"}
	current := CustomProviders()
	if _, ok := current["snapshot"]; !ok {
		t.Fatal("mutating a snapshot removed the registered provider")
	}
	if _, ok := current["injected"]; ok {
		t.Fatal("mutating a snapshot injected a provider into the registry")
	}
}

func TestCustomProvidersConcurrentReload(t *testing.T) {
	preserveUserModelRegistries(t)
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{"providers":{"first":{"baseUrl":"https://first.invalid/v1"},"second":{"baseUrl":"https://second.invalid/v1"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	LoadUserModels(path)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(4)
	for worker := range 4 {
		go func() {
			defer wg.Done()
			<-start
			for range 100 {
				if worker == 0 {
					LoadUserModels(path)
					continue
				}
				snapshot := CustomProviders()
				if len(snapshot) != 2 || snapshot["first"].BaseURL != "https://first.invalid/v1" || snapshot["second"].BaseURL != "https://second.invalid/v1" {
					t.Error("reload exposed an incomplete registry snapshot")
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
}
