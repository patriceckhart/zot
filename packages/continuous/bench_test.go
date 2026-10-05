package continuous

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/core"
)

// Benchmarks report Go's aggregate measurements for the documented workloads.
// They are inputs to the published methodology (hardware, OS, backend,
// durability mode, workload) and not acceptance results by themselves. Run:
//
//	go test ./packages/continuous -run '^$' -bench . -benchmem

// BenchmarkRunRoundTrip measures one submission answered by a synthetic
// provider through the service: admission, request intent, response, settle.
func BenchmarkRunRoundTrip(b *testing.B) {
	for _, mode := range []string{"memory", "journal-process"} {
		b.Run(mode, func(b *testing.B) {
			ctx := context.Background()
			var store storage.Store
			if mode == "memory" {
				store = newMemoryStore()
			} else {
				var err error
				store, err = journal.Open(ctx, filepath.Join(b.TempDir(), "store"), journal.Options{Durability: storage.Process})
				if err != nil {
					b.Fatal(err)
				}
			}
			r, _ := New(store)
			defer r.Close()
			svc, _ := NewService(r, echoEngine(&echoClient{}, core.NewRegistry()), ExecutionOptions{PartialFlushInterval: -1})
			c, _ := r.OpenRoot(ctx, "bench", AgentConfig{Model: "echo"})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := r.Submit(ctx, c.ID, "bench", "", fmt.Sprintf("message %d", i)); err != nil {
					b.Fatal(err)
				}
				if _, _, err := svc.Step(ctx, c.ID); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkResumeLongConversation measures building the model context of a
// conversation with many entries, which is what a resume pays before its
// first request. It reports the per-resume latency at a fixed size.
func BenchmarkResumeLongConversation(b *testing.B) {
	ctx := context.Background()
	for _, n := range []int{1000, 4000} {
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			r, _ := New(newMemoryStore())
			defer r.Close()
			svc, _ := NewService(r, echoEngine(&echoClient{}, core.NewRegistry()), ExecutionOptions{PartialFlushInterval: -1})
			c, _ := r.OpenRoot(ctx, "bench", AgentConfig{Model: "echo"})
			for i := 0; i < n/2; i++ {
				r.Submit(ctx, c.ID, "bench", "", "x")
				svc.Step(ctx, c.ID)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				snap, _ := r.Snapshot(ctx)
				if _, err := ModelContext(ctx, snap, c.ID, 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkWaitingConversationsIdle measures the host's idle memory and
// goroutine count with many conversations that have nothing to do: waiting
// work must not occupy workers.
func BenchmarkWaitingConversationsIdle(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := New(newMemoryStore())
	defer r.Close()
	host, _ := NewHost(r, echoEngine(&echoClient{}, core.NewRegistry()), HostOptions{Execution: ExecutionOptions{PartialFlushInterval: -1}})
	go host.Run(ctx)
	for i := 0; i < 100; i++ {
		c, _ := r.OpenRoot(ctx, fmt.Sprintf("bench-%d", i), AgentConfig{Model: "echo"})
		r.Submit(ctx, c.ID, "bench", "", "hello")
	}
	host.Nudge()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m, _ := host.Metrics(ctx)
		if m.QueuedInputs == 0 && m.ActiveRuns == 0 && m.ActiveSteps == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := host.Metrics(ctx); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	b.ReportMetric(float64(runtime.NumGoroutine()), "goroutines")
	b.ReportMetric(float64(ms.HeapAlloc)/1024/1024, "heap_mib")
}
