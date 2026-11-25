package httpapi

import (
	"net/http"
	"time"

	"cs-agent/dbbackup"
)

// StoreBackupStatus reports the off-node SQLite backup runner's state.
type StoreBackupStatus interface {
	Status() dbbackup.Status
}

// handleAdminStoreBackupGet returns the off-node backup status. With backups
// off the answer is {"enabled": false}; enabled but misconfigured backups answer
// enabled=true with the setup error in last_error.
func (s *Server) handleAdminStoreBackupGet(w http.ResponseWriter, _ *http.Request, _ scope) {
	if s.cfg.StoreBackup == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.StoreBackup.Status())
}

type configErrorStatus struct{ st dbbackup.Status }

func (c configErrorStatus) Status() dbbackup.Status { return c.st }

// StoreBackupConfigError is the status of backups that are enabled but failed
// to set up: enabled=true, with last_error "config: <err>" and last_error_at
// set to at.
func StoreBackupConfigError(err error, at time.Time) StoreBackupStatus {
	at = at.UTC()
	return configErrorStatus{dbbackup.Status{Enabled: true, LastError: "config: " + err.Error(), LastErrorAt: &at}}
}
