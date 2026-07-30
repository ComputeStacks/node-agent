package backup

import (
	"context"
	"cs-agent/store"
	"testing"
)

// TestCompactActionFor pins the compact sweep's three-way decision against a real
// store, and above all that a store that could not answer is never read as "no
// repository". That conflation is the one mistake here with a lasting cost: it would
// skip a healthy volume's compaction, silently, for as long as the store kept
// failing, while the honest answer costs only the current sweep.
func TestCompactActionFor(t *testing.T) {
	ctx := context.Background()

	t.Run("row present compacts", func(t *testing.T) {
		st := testStore(t)
		if err := st.UpsertRepository(ctx, store.Repository{Name: "vol-1", Archives: []string{"a1"}}); err != nil {
			t.Fatalf("upsert repository: %v", err)
		}
		action, err := compactActionFor(ctx, st, "vol-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if action != compactRun {
			t.Fatalf("action = %d, want compactRun (%d)", action, compactRun)
		}
	})

	t.Run("no row skips", func(t *testing.T) {
		st := testStore(t)
		// A volume that has never been backed up: GetRepository reports sql.ErrNoRows
		// as found=false with a nil error, and there is no repository to compact.
		action, err := compactActionFor(ctx, st, "never-backed-up")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if action != compactSkipNoRepo {
			t.Fatalf("action = %d, want compactSkipNoRepo (%d)", action, compactSkipNoRepo)
		}
	})

	t.Run("store error is not no repository", func(t *testing.T) {
		st := testStore(t)
		if err := st.UpsertRepository(ctx, store.Repository{Name: "vol-1", Archives: []string{"a1"}}); err != nil {
			t.Fatalf("upsert repository: %v", err)
		}
		// Closing the store makes the query fail rather than miss, over a volume that
		// demonstrably DOES have a repository — so anything but compactSkipStoreError
		// here is a healthy volume being skipped on a store fault.
		if err := st.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
		action, err := compactActionFor(ctx, st, "vol-1")
		if err == nil {
			t.Fatal("querying a closed store returned no error")
		}
		if action != compactSkipStoreError {
			t.Fatalf("action = %d, want compactSkipStoreError (%d)", action, compactSkipStoreError)
		}
	})
}
