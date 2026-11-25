// Package dbbackup uploads encrypted snapshots of the agent's SQLite databases
// to object storage and prunes them on a tiered schedule.
package dbbackup

import (
	"path/filepath"
	"strings"
	"time"
)

const tsLayout = "20060102T150405Z"

const keySuffix = ".db.gz.age"

// KeyFor builds the object key for one snapshot. db is "control" or
// "projects/<id>"; lineage is control.db's instance_id at the time of the run.
func KeyFor(node, db, lineage string, ts time.Time) string {
	return "nodes/" + node + "/" + db + "/" + ts.UTC().Format(tsLayout) + "_" + lineage + keySuffix
}

// ParseKey is the inverse of KeyFor. It tolerates a leading storage prefix and
// returns ok=false for anything that is not a snapshot key.
func ParseKey(key string) (node, db, lineage string, ts time.Time, ok bool) {
	segs := strings.Split(key, "/")
	n := len(segs)
	if n < 4 {
		return
	}
	file := segs[n-1]
	switch {
	case n >= 5 && segs[n-3] == "projects" && segs[n-5] == "nodes":
		node, db = segs[n-4], "projects/"+segs[n-2]
	case segs[n-2] == "control" && segs[n-4] == "nodes":
		node, db = segs[n-3], "control"
	default:
		return "", "", "", time.Time{}, false
	}
	if node == "" || strings.HasSuffix(db, "/") || db == "projects/" {
		return "", "", "", time.Time{}, false
	}
	if !strings.HasSuffix(file, keySuffix) {
		return "", "", "", time.Time{}, false
	}
	base := strings.TrimSuffix(file, keySuffix)
	i := strings.Index(base, "_")
	if i != len(tsLayout) || i == len(base)-1 {
		return "", "", "", time.Time{}, false
	}
	t, err := time.Parse(tsLayout, base[:i])
	if err != nil {
		return "", "", "", time.Time{}, false
	}
	return node, db, base[i+1:], t.UTC(), true
}

// StatePath is where the runner keeps its bookkeeping. The restore command
// deletes this file so the next run re-uploads everything.
func StatePath(dataDir string) string {
	return filepath.Join(dataDir, "backup-state.json")
}
