package agent

import (
	"bufio"
	"context"
	"io"
	"testing"
	"time"
)

func TestContinuousWorkerCancellation(t *testing.T) {
	for _, blocked := range []string{"read", "write"} {
		t.Run(blocked, func(t *testing.T) {
			root := t.TempDir()
			inR, inW := io.Pipe()
			outR, outW := io.Pipe()
			defer inR.Close()
			defer inW.Close()
			defer outR.Close()
			defer outW.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			exited := make(chan struct{})
			go func() {
				defer close(exited)
				done <- runContinuousWorker(ctx, []string{"--root", root}, inR, outW)
			}()
			t.Cleanup(func() {
				cancel()
				inR.Close()
				outW.Close()
				<-exited
			})
			if _, err := io.WriteString(inW, "{\"id\":\"1\",\"method\":\"hello\"}\n"); err != nil {
				t.Fatal(err)
			}
			if blocked == "read" {
				if _, err := bufio.NewReader(outR).ReadBytes('\n'); err != nil {
					t.Fatal(err)
				}
			}
			// Keep the peer endpoints open. Cancellation must unblock either
			// the next request read or the pending hello response write.
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not stop on cancellation")
			}
		})
	}
}
