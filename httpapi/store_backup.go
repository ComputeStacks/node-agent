package httpapi

import (
	"net/http"

	"cs-agent/dbbackup"
)

// StoreBackupStatus reports the off-node SQLite backup runner's state.
type StoreBackupStatus interface {
	Status() dbbackup.Status
}

// handleAdminStoreBackupGet returns the off-node backup status. With backups
// off the answer is {"enabled": false}.
func (s *Server) handleAdminStoreBackupGet(w http.ResponseWriter, _ *http.Request, _ scope) {
	if s.cfg.StoreBackup == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.StoreBackup.Status())
}
