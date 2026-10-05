package journal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// A current journal inspects as current and migratable; migrating it is a
// verified rewrite with identical records, the source is unchanged, and the
// destination opens as a writer with the archived revision and epoch.
func TestInspectFormatAndMigrateCurrent(t *testing.T) {
	ctx := context.Background()
	source := filepath.Join(t.TempDir(), "store")
	s := openTest(t, source)
	commitTest(t, s)
	commitTest(t, s)
	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	epoch := s.Epoch()
	if _, err := InspectFormat(ctx, source); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("inspection bypassed active writer: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	format, err := InspectFormat(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if format.Schema != CurrentSchema || !format.Current || !format.Migratable || format.Revision != snap.Revision() || format.Epoch != epoch {
		t.Fatalf("format: %+v", format)
	}
	before := journalFiles(t, source)
	destination := filepath.Join(t.TempDir(), "migrated")
	report, err := Migrate(ctx, source, destination, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Valid || report.Schema != CurrentSchema || report.Revision != snap.Revision() || report.Epoch != epoch || report.Scope != "journal" {
		t.Fatalf("migration report: %+v", report)
	}
	assertJournalFiles(t, source, before)
	verified, err := Verify(ctx, destination)
	if err != nil || !verified.Valid || verified.Revision != snap.Revision() {
		t.Fatalf("destination verification: %+v %v", verified, err)
	}
	// Destination must not exist; the source must not be its own target.
	if _, err := Migrate(ctx, source, destination, testOptions()); err == nil {
		t.Fatal("existing destination accepted")
	}
	if _, err := Migrate(ctx, source, filepath.Join(source, "inner"), testOptions()); err == nil {
		t.Fatal("destination inside the source accepted")
	}
	migrated, err := Open(ctx, destination, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	view, err := migrated.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision() != snap.Revision()+1 || migrated.Epoch() != epoch+1 {
		t.Fatalf("migrated open: revision %d epoch %d", view.Revision(), migrated.Epoch())
	}
	value, ok := view.Get("test")
	if !ok || string(value) != `"synthetic"` {
		t.Fatalf("migrated record: %s %v", value, ok)
	}
}

// A journal written with an unknown schema is reported as not current and
// not migratable, Open refuses it as corrupt, and Migrate reports the schema
// as unsupported rather than rewriting it.
func TestMigrateUnsupportedSchema(t *testing.T) {
	ctx := context.Background()
	source := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "writer.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writeVerificationFixture(t, source, []storage.Commit{
		{Schema: 7, Revision: 1, Epoch: 1},
		{Schema: 7, Revision: 2, Epoch: 1, Operations: []storage.Operation{{Key: "a", Value: json.RawMessage(`1`)}}},
	})
	format, err := InspectFormat(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if format.Schema != 7 || format.Current || format.Migratable || format.Revision != 2 {
		t.Fatalf("format: %+v", format)
	}
	if _, err := Verify(ctx, source); !errors.Is(err, storage.ErrCorrupt) {
		t.Fatalf("verify of unknown schema: %v", err)
	}
	destination := filepath.Join(t.TempDir(), "migrated")
	if _, err := Migrate(ctx, source, destination, testOptions()); !errors.Is(err, ErrSchemaUnsupported) {
		t.Fatalf("unsupported schema migrated: %v", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed migration left a destination")
	}
}

// A registered migration converts every commit, validation applies the
// current rules to the converted commits, and a converter error leaves no
// partial destination.
func TestMigrateRegisteredSchema(t *testing.T) {
	ctx := context.Background()
	// A synthetic older schema whose commits carried records under a
	// "legacy/" prefix; the migration renames them.
	const legacy = 100
	migrations[legacy] = func(c storage.Commit) (storage.Commit, error) {
		for i := range c.Operations {
			if c.Operations[i].Key == "legacy/fail" {
				return c, errors.New("cannot convert")
			}
			c.Operations[i].Key = "record/" + c.Operations[i].Key
		}
		return c, nil
	}
	t.Cleanup(func() { delete(migrations, legacy) })
	source := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "writer.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writeVerificationFixture(t, source, []storage.Commit{
		{Schema: legacy, Revision: 1, Epoch: 1},
		{Schema: legacy, Revision: 2, Epoch: 1, Operations: []storage.Operation{{Key: "legacy/a", Value: json.RawMessage(`1`)}}},
		{Schema: legacy, Revision: 3, Epoch: 2, Operations: []storage.Operation{{Key: "legacy/a", Delete: true}, {Key: "legacy/b", Value: json.RawMessage(`{"x":2}`)}}},
	})
	format, err := InspectFormat(ctx, source)
	if err != nil || format.Current || !format.Migratable || format.Schema != legacy {
		t.Fatalf("format: %+v %v", format, err)
	}
	destination := filepath.Join(t.TempDir(), "migrated")
	report, err := Migrate(ctx, source, destination, testOptions())
	if err != nil || !report.Valid || report.Revision != 3 || report.Epoch != 2 {
		t.Fatalf("migrate: %+v %v", report, err)
	}
	migrated, err := Open(ctx, destination, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	view, err := migrated.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := view.Get("record/legacy/a"); ok {
		t.Fatal("deleted record survived migration")
	}
	if b, ok := view.Get("record/legacy/b"); !ok || string(b) != `{"x":2}` {
		t.Fatalf("migrated record: %s %v", b, ok)
	}
	commits, err := migrated.Scan(ctx, 0, 10)
	if err != nil || len(commits) != 4 || commits[0].Schema != CurrentSchema || commits[2].Operations[1].Key != "record/legacy/b" {
		t.Fatalf("migrated history: %d %v", len(commits), err)
	}

	// Converter failure: no destination remains.
	failing := filepath.Join(t.TempDir(), "failing")
	if err := os.Mkdir(failing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(failing, "writer.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writeVerificationFixture(t, failing, []storage.Commit{
		{Schema: legacy, Revision: 1, Epoch: 1},
		{Schema: legacy, Revision: 2, Epoch: 1, Operations: []storage.Operation{{Key: "legacy/fail", Value: json.RawMessage(`1`)}}},
	})
	failed := filepath.Join(t.TempDir(), "failed")
	if _, err := Migrate(ctx, failing, failed, testOptions()); err == nil {
		t.Fatal("failing converter accepted")
	}
	if _, err := os.Stat(failed); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed migration left a destination")
	}
}
