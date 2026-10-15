package backup

import (
	"context"
	"testing"

	"cs-agent/store"
	"cs-agent/types"
)

// A backup for a volume marked for deletion returns before FindRepository/Setup,
// so it never re-creates a repository the trash task destroyed (and never reaches
// docker, which is what makes this test runnable without it).
func TestPerform_SkipsTrashVolume(t *testing.T) {
	st := testStore(t)
	putVol(t, st, types.Volume{Name: "v1", Node: "test-node", Backup: true, Trash: true, ProjectID: 7})
	err := Perform(context.Background(), st, store.Task{ID: "b1", Name: "volume.backup", Node: "test-node", Volume: "v1"}, nil)
	if err != nil {
		t.Fatalf("Perform on trash volume: %v", err)
	}
}
