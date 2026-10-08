package journal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage"
)

const crashExitCode = 86

func TestInitializeDataCreateFailureReleasesFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	failure := errors.New("synthetic failure after creating data file")
	s, err := open(context.Background(), path, Options{Durability: storage.Process}, func(point string) error {
		if point == "after.open.data.create" {
			return failure
		}
		return nil
	})
	if s != nil {
		s.Close()
		t.Fatal("failed initialization returned a store")
	}
	if !errors.Is(err, failure) {
		t.Fatalf("initialization failure: %v", err)
	}
	// Windows refuses removal while a leaked handle is still open. Failed
	// initialization must release both the data file and the writer lock.
	for _, name := range []string{"commits.log", "writer.lock"} {
		if err := os.Remove(filepath.Join(path, name)); err != nil {
			t.Fatalf("failed initialization kept %s open: %v", name, err)
		}
	}
}

// Discover checkpoints from the real path, rather than maintaining a matrix
// which can silently omit newly added persistence operations.
func collectFaultPoints(t *testing.T, opts Options, stage string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store")
	if stage != "initialize" {
		seedFaultStore(t, path, opts)
	}
	var points []string
	armed := stage != "submit"
	s, err := open(context.Background(), path, opts, func(point string) error {
		if armed {
			points = append(points, point)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if stage == "submit" {
		r, err := continuous.New(s)
		if err != nil {
			t.Fatal(err)
		}
		c, err := r.OpenRoot(context.Background(), "crash-workspace", continuous.AgentConfig{})
		if err != nil {
			t.Fatal(err)
		}
		armed = true
		if _, err := r.Submit(context.Background(), c.ID, "uncertain", "retry-key", "synthetic input"); err != nil {
			t.Fatal(err)
		}
	}
	if len(points) == 0 {
		t.Fatal("no persistence checkpoints")
	}
	seen := make(map[string]bool)
	for i, point := range points {
		if seen[point] {
			t.Fatalf("ambiguous checkpoint %s", point)
		}
		seen[point] = true
		if i%2 == 0 && !strings.HasPrefix(point, "before.") {
			t.Fatalf("missing before checkpoint: %s", point)
		}
		if i%2 == 1 && point != "after."+strings.TrimPrefix(points[i-1], "before.") {
			t.Fatalf("unpaired checkpoint: %s", point)
		}
	}
	if len(points)%2 != 0 || !seen["after.commit.boundary.rename"] {
		t.Fatal("incomplete persistence checkpoints")
	}
	return points
}

type faultSeed struct {
	submission continuous.Submission
	epoch      uint64
}

func seedFaultStore(t *testing.T, path string, opts Options) faultSeed {
	t.Helper()
	s, err := Open(context.Background(), path, opts)
	if err != nil {
		t.Fatal(err)
	}
	r, err := continuous.New(s)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	defer r.Close()
	c, err := r.OpenRoot(context.Background(), "crash-workspace", continuous.AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := r.Submit(context.Background(), c.ID, "baseline", "accepted-key", "acknowledged input")
	if err != nil {
		t.Fatal(err)
	}
	return faultSeed{baseline, s.Epoch()}
}

func faultModes() []storage.Durability {
	modes := []storage.Durability{storage.Process}
	if strictSupported {
		modes = append(modes, storage.Strict)
	}
	return modes
}

// The child exits inside the actual filesystem path. It intentionally runs no
// cleanup, testing OS lock release and on-disk recovery rather than Close.
func TestPersistenceCrashChild(t *testing.T) {
	path := os.Getenv("ZOT_JOURNAL_FAULT_PATH")
	if path == "" {
		return
	}
	point := os.Getenv("ZOT_JOURNAL_FAULT_POINT")
	stage := os.Getenv("ZOT_JOURNAL_FAULT_STAGE")
	opts := Options{Durability: storage.Durability(os.Getenv("ZOT_JOURNAL_FAULT_MODE"))}
	armed := stage != "submit"
	s, err := open(context.Background(), path, opts, func(p string) error {
		if armed && p == point {
			os.Exit(crashExitCode)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stage == "submit" {
		r, err := continuous.New(s)
		if err != nil {
			t.Fatal(err)
		}
		c, err := r.OpenRoot(context.Background(), "crash-workspace", continuous.AgentConfig{})
		if err != nil {
			t.Fatal(err)
		}
		armed = true
		if _, err := r.Submit(context.Background(), c.ID, "uncertain", "retry-key", "synthetic input"); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	t.Fatalf("crash checkpoint not reached: %s", point)
}

func TestPersistenceCrashMatrix(t *testing.T) {
	for _, mode := range faultModes() {
		for _, stage := range []string{"initialize", "reopen", "submit"} {
			opts := Options{Durability: mode}
			for _, point := range collectFaultPoints(t, opts, stage) {
				t.Run(string(mode)+"/"+stage+"/"+point, func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "store")
					var seed faultSeed
					if stage != "initialize" {
						seed = seedFaultStore(t, path, opts)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPersistenceCrashChild$")
					cmd.Env = append(os.Environ(), "ZOT_JOURNAL_FAULT_PATH="+path, "ZOT_JOURNAL_FAULT_POINT="+point, "ZOT_JOURNAL_FAULT_STAGE="+stage, "ZOT_JOURNAL_FAULT_MODE="+string(mode))
					b, err := cmd.CombinedOutput()
					var exit *exec.ExitError
					if !errors.As(err, &exit) || exit.ExitCode() != crashExitCode {
						t.Fatalf("child did not reach checkpoint: %v, %s", err, b)
					}
					checkFaultRecovery(t, path, opts, stage, point, seed)
				})
			}
		}
	}
}

func TestPersistenceErrorMatrix(t *testing.T) {
	failure := errors.New("synthetic persistence failure")
	for _, mode := range faultModes() {
		for _, stage := range []string{"initialize", "reopen", "submit"} {
			opts := Options{Durability: mode}
			for _, point := range collectFaultPoints(t, opts, stage) {
				t.Run(string(mode)+"/"+stage+"/"+point, func(t *testing.T) {
					ctx := context.Background()
					path := filepath.Join(t.TempDir(), "store")
					var seed faultSeed
					if stage != "initialize" {
						seed = seedFaultStore(t, path, opts)
					}
					armed := stage != "submit"
					s, err := open(ctx, path, opts, func(p string) error {
						if armed && p == point {
							return failure
						}
						return nil
					})
					if stage == "submit" {
						if err != nil {
							t.Fatal(err)
						}
						defer s.Close()
						r, err := continuous.New(s)
						if err != nil {
							t.Fatal(err)
						}
						c, err := r.OpenRoot(ctx, "crash-workspace", continuous.AgentConfig{})
						if err != nil {
							t.Fatal(err)
						}
						snap, err := s.Snapshot(ctx)
						if err != nil {
							t.Fatal(err)
						}
						armed = true
						_, err = r.Submit(ctx, c.ID, "uncertain", "retry-key", "synthetic input")
						if !errors.Is(err, failure) {
							t.Fatalf("failure not propagated: %v", err)
						}
						if _, err := s.Snapshot(ctx); !errors.Is(err, storage.ErrClosed) {
							t.Fatalf("failed writer readable: %v", err)
						}
						if _, err := s.Commit(ctx, storage.Mutation{}); !errors.Is(err, storage.ErrClosed) {
							t.Fatalf("failed writer writable: %v", err)
						}
						if err := s.Wait(ctx, snap.Revision()); !errors.Is(err, storage.ErrClosed) {
							t.Fatalf("failed writer wait did not stop: %v", err)
						}
						other, err := Open(ctx, path, opts)
						if other != nil {
							other.Close()
						}
						if !errors.Is(err, storage.ErrLocked) {
							t.Fatalf("failed writer released lock before close: %v", err)
						}
						if err := s.Close(); err != nil {
							t.Fatal(err)
						}
					} else if !errors.Is(err, failure) {
						if s != nil {
							s.Close()
						}
						t.Fatalf("open failure not propagated: %v", err)
					}
					checkFaultRecovery(t, path, opts, stage, point, seed)
				})
			}
		}
	}
}

func checkFaultRecovery(t *testing.T, path string, opts Options, stage, point string, seed faultSeed) {
	t.Helper()
	ctx := context.Background()
	// An interrupted first creation with only one authority file is rejected,
	// not silently converted to a fresh store. No input was accepted in this case.
	_, dataErr := os.Stat(filepath.Join(path, "commits.log"))
	_, boundaryErr := os.Stat(filepath.Join(path, "boundary"))
	s, err := Open(ctx, path, opts)
	if stage == "initialize" && dataErr == nil && errors.Is(boundaryErr, os.ErrNotExist) {
		if s != nil {
			s.Close()
		}
		if !errors.Is(err, storage.ErrCorrupt) {
			t.Fatalf("incomplete creation did not fail closed: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if stage == "initialize" {
		commitTest(t, s)
		return
	}
	if s.Epoch() <= seed.epoch {
		t.Fatal("recovery did not fence previous writer")
	}
	r, err := continuous.New(s)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.OpenRoot(ctx, "crash-workspace", continuous.AgentConfig{})
	if err != nil || c.ID != seed.submission.ConversationID {
		t.Fatalf("root changed: %v", err)
	}
	baseline, err := r.Submit(ctx, c.ID, "baseline", "accepted-key", "acknowledged input")
	if err != nil || !reflect.DeepEqual(baseline, seed.submission) {
		t.Fatalf("acknowledged admission changed: %v", err)
	}
	if stage == "submit" {
		// In an ordinary process crash the completed rename is observable on
		// reopen. This assertion does not simulate power loss or cache rollback.
		published := point == "after.commit.boundary.rename" || strings.HasSuffix(point, ".commit.directory.sync") || strings.HasSuffix(point, ".commit.device.sync")
		wantSequence := uint64(1)
		if published {
			wantSequence = 2
		}
		if c.QueueSequence != wantSequence {
			t.Fatalf("queue state at %s: got %d, want %d", point, c.QueueSequence, wantSequence)
		}
		snap, err := s.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// All admission records must be present together, or all absent. The
		// first input is claimed by a chain at admission; later ones queue
		// behind it, so queue rows count only the inputs after the first.
		for _, prefix := range []string{"submission/", "entry/" + c.ID + "/", "dedup/submit/"} {
			rows, err := snap.Page(prefix, "", 100)
			if err != nil || len(rows) != int(c.QueueSequence) {
				t.Fatalf("partial admission for %s: %d, %v", prefix, len(rows), err)
			}
		}
		if rows, err := snap.Page("queue/"+c.ID+"/", "", 100); err != nil || len(rows) != int(c.QueueSequence)-1 {
			t.Fatalf("partial admission for the queue: %d, %v", len(rows), err)
		}
		var persisted continuous.Submission
		if c.QueueSequence == 2 {
			rows, _ := snap.Page("submission/", "", 100)
			for _, row := range rows {
				var sub continuous.Submission
				if err := json.Unmarshal(row.Value, &sub); err != nil {
					t.Fatal(err)
				}
				if sub.Actor == "uncertain" {
					persisted = sub
				}
			}
			if persisted.ID == "" {
				t.Fatal("missing persisted uncertain submission")
			}
		}
		first, err := r.Submit(ctx, c.ID, "uncertain", "retry-key", "synthetic input")
		if err != nil || first.Sequence != 2 {
			t.Fatalf("retry admission: %v", err)
		}
		if persisted.ID != "" && !reflect.DeepEqual(first, persisted) {
			t.Fatal("retry changed committed uncertain admission")
		}
		second, err := r.Submit(ctx, c.ID, "uncertain", "retry-key", "synthetic input")
		if err != nil || !reflect.DeepEqual(second, first) {
			t.Fatalf("retry duplicated work: %v", err)
		}
		if _, err := r.Submit(ctx, c.ID, "uncertain", "retry-key", "different input"); !errors.Is(err, continuous.ErrRequestConflict) {
			t.Fatalf("conflicting retry accepted: %v", err)
		}
		c, err = r.Conversation(ctx, c.ID)
		if err != nil || c.QueueSequence != 2 || c.EntrySequence != 2 {
			t.Fatalf("retry changed sequence: %v", err)
		}
	}
}
