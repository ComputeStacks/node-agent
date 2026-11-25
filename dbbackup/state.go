package dbbackup

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// marker is the cheap change-detection fingerprint of a database: size and
// mtime of X.db and X.db-wal. A missing -wal equals a zero-length one; -shm is
// ignored.
type marker struct {
	Valid     bool  `json:"valid"`
	Size      int64 `json:"size"`
	MtimeNs   int64 `json:"mtime_ns"`
	WalSize   int64 `json:"wal_size"`
	WalMtimeN int64 `json:"wal_mtime_ns"`
}

type dbState struct {
	Marker     marker `json:"marker"`
	Hash       string `json:"hash"`
	Key        string `json:"key"`
	Lineage    string `json:"lineage"`
	UploadedAt int64  `json:"uploaded_at"`
}

type stateFile struct {
	DBs             map[string]*dbState `json:"dbs"`
	LastRetentionAt int64               `json:"last_retention_at"`
	Status          Status              `json:"status"`
}

func newState() *stateFile { return &stateFile{DBs: map[string]*dbState{}} }

func takeMarker(dbPath string) marker {
	fi, err := os.Stat(dbPath)
	if err != nil {
		return marker{}
	}
	m := marker{Valid: true, Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano()}
	if wi, err := os.Stat(dbPath + "-wal"); err == nil && wi.Size() > 0 {
		m.WalSize, m.WalMtimeN = wi.Size(), wi.ModTime().UnixNano()
	}
	return m
}

// loadState never fails: a missing or corrupt file yields an empty state (and
// corrupt is reported so the caller can log it).
func loadState(path string) (st *stateFile, corrupt error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			corrupt = err
		}
		return newState(), corrupt
	}
	st = newState()
	if err := json.Unmarshal(b, st); err != nil {
		return newState(), err
	}
	if st.DBs == nil {
		st.DBs = map[string]*dbState{}
	}
	for k, v := range st.DBs {
		if v == nil {
			delete(st.DBs, k)
		}
	}
	return st, nil
}

func saveState(path string, st *stateFile) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".backup-state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func(e error) error { f.Close(); os.Remove(tmp); return e }
	if err := f.Chmod(0o600); err != nil {
		return cleanup(err)
	}
	if _, err := f.Write(b); err != nil {
		return cleanup(err)
	}
	if err := f.Sync(); err != nil {
		return cleanup(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
