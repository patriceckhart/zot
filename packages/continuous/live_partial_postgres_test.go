package continuous

import (
	"context"
	"os"
	"testing"
)

func TestPostgresAttachedRAMPreviewWithoutCheckpoint(t *testing.T) {
	if os.Getenv("ZOT_TEST_POSTGRES_DSN") == "" {
		if os.Getenv("ZOT_TEST_POSTGRES_REQUIRED") != "" {
			t.Fatal("PostgreSQL fixture required but not configured")
		}
		t.Skip("ZOT_TEST_POSTGRES_DSN not set")
	}
	testAttachedRAMPreviewWithoutCheckpoint(t, func(t *testing.T) *Runtime {
		store, err := openCrashStore(context.Background(), "postgres", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		r, err := New(store)
		if err != nil {
			store.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		return r
	})
}
