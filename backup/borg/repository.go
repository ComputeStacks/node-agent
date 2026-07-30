/*
Borg Repository Manager

Will use borg container:
* FindRepository
* Setup
* Info
* Contents

Uses local borg installation:
* Delete
* Prune
*/
package borg

import (
	"context"
	"cs-agent/sshremote"
	"cs-agent/store"
	"cs-agent/types"
	"reflect"
	"strconv"

	"github.com/spf13/viper"

	"github.com/getsentry/sentry-go"
)

func FindRepository(st *store.Store, vol *types.Volume, source *types.Volume) (*Repository, *LogMessage) {
	r := Repository{Name: vol.Name, Retention: vol.Retention, SourceVolumeName: source.Name, Store: st}

	containerBuilt, containerErr := r.InitBackupContainer(vol, source)
	if containerErr != nil {
		sentry.CaptureException(containerErr)
		return nil, &LogMessage{Message: containerErr.Error()}
	}
	if !containerBuilt {
		return nil, &LogMessage{Message: "Failed to build backup container"}
	}

	// Find Repo
	repoResponse, err := r.Info()
	if err != nil {
		r.StopContainer()
		return nil, err
	}

	if repoResponse == (RepositoryResponse{}) {
		r.StopContainer()
		return nil, &LogMessage{MsgID: "Repository.DoesNotExist", Message: "Missing Repository"}
	}

	return &r, nil
}

func (r *Repository) FindArchive(name string) (a *Archive, err *LogMessage) {
	a = &Archive{Name: name, Repository: r}
	// Attempt to load archive. Nil = not exist.
	if _, err = a.Info(); err != nil {
		return nil, err
	}
	return a, nil
}

func (r *Repository) Setup(vol *types.Volume, source *types.Volume) *LogMessage {
	if reflect.ValueOf(r.Container).IsNil() {
		containerBuilt, containerErr := r.InitBackupContainer(vol, source)
		if containerErr != nil {
			sentry.CaptureException(containerErr)
			return &LogMessage{Message: containerErr.Error()}
		}
		if !containerBuilt {
			return &LogMessage{Message: "Failed to build backup container"}
		}
	}

	var backupCmd []string

	backupCmd = append(backupCmd, "borg --log-json")
	backupCmd = append(backupCmd, "--lock-wait "+viper.GetString("backups.borg.lock_wait"))
	backupCmd = append(backupCmd, "init --error --encryption=repokey-blake2")

	if _, _, log := r.ExecWithLog(backupCmd); log != (LogMessage{}) {
		return &log
	}

	// Register the (now-initialized) repository's observed state in control.db.
	r.Sync()
	return nil
}

func (r *Repository) Info() (RepositoryResponse, *LogMessage) {
	if reflect.ValueOf(r.Container).IsNil() {
		return RepositoryResponse{}, &LogMessage{Message: "Missing backup container"}
	}

	cmd := []string{"borg --log-json"}
	cmd = append(cmd, "--lock-wait "+viper.GetString("backups.borg.lock_wait"))
	cmd = append(cmd, "info --error --json")

	_, response, logMsg := r.ExecWithLog(cmd)

	if logMsg != (LogMessage{}) {
		return RepositoryResponse{}, &logMsg
	}

	repoResponse, repoLog := readRepoResponse(response)

	if repoLog != nil {
		return RepositoryResponse{}, repoLog
	}

	return repoResponse, nil
}

func (r *Repository) Contents() (RepositoryContentResponse, *LogMessage) {
	if reflect.ValueOf(r.Container).IsNil() {
		return RepositoryContentResponse{}, &LogMessage{Message: "Missing backup container"}
	}

	cmd := []string{"borg --log-json"}
	cmd = append(cmd, "--lock-wait "+viper.GetString("backups.borg.lock_wait"))
	cmd = append(cmd, "list --error --json")

	res := r.RunBorg("borg list", cmd)

	if res.Failure != nil {
		return RepositoryContentResponse{}, res.Failure
	}

	repoResponse, repoLog := readRepoContentResponse(res.Response)

	if repoLog != nil {
		return RepositoryContentResponse{}, repoLog
	}

	return repoResponse, nil
}

func (r *Repository) Delete() (bool, error) {
	vol := types.Volume{Name: r.Name, Trash: true}
	return r.TrashBackupVolumeExists(&vol)
}

/*
*

		Prune Repository

		*  Will ignore all repositories that don't match the `auto-` prefix.
	    *  Testing this by creating 2 backups back-to-back, and then running prune with
		   an hourly retention of 2 will only retain 1 because the content would not have changed between the 2 backups.
*/
func (r *Repository) Prune() *LogMessage {
	vol := types.Volume{Name: r.Name, Trash: true}
	sourceVol := types.Volume{Name: r.SourceVolumeName, Trash: true}
	if reflect.ValueOf(r.Container).IsNil() {
		containerBuilt, containerErr := r.InitBackupContainer(&vol, &sourceVol)
		if containerErr != nil {
			sentry.CaptureException(containerErr)
			return &LogMessage{Message: containerErr.Error()}
		}
		if !containerBuilt {
			return &LogMessage{Message: "Failed to build backup container"}
		}
	}

	cmd := []string{"borg --log-json"}
	cmd = append(cmd, "--lock-wait "+viper.GetString("backups.borg.lock_wait"))
	cmd = append(cmd, "prune --error --stats --prefix=\"auto-\"")
	cmd = append(cmd, "--keep-hourly="+strconv.Itoa(r.Retention.Hourly))
	cmd = append(cmd, "--keep-daily="+strconv.Itoa(r.Retention.Daily))
	cmd = append(cmd, "--keep-weekly="+strconv.Itoa(r.Retention.Weekly))
	cmd = append(cmd, "--keep-monthly="+strconv.Itoa(r.Retention.Monthly))
	cmd = append(cmd, "--keep-yearly="+strconv.Itoa(r.Retention.Annually))

	if _, _, log := r.ExecWithLog(cmd); log != (LogMessage{}) {
		return &log
	}

	r.Sync()
	borgLogger().Info("Completed prune event", "volume_name", r.Name)
	return nil
}

// Compact reclaims space freed by prune/delete. Callers MUST hold the per-repo
// lock (AcquireRepoLock) so a compact never overlaps an export of the same repo
// (export reads with --bypass-lock and would fail on a segment compact rewrites).
//
// For the NFS backend compaction runs LOCALLY on the backup server over SSH:
// rewriting segments through the NFS mount would push all that I/O over the
// network. local/SSH backends compact through the borg container (for the SSH
// backend borg-serve keeps the heavy work server-side).
func (r *Repository) Compact() *LogMessage {
	if viper.GetBool("backups.borg.nfs") {
		return r.compactNFS()
	}
	return r.compactContainer()
}

func (r *Repository) compactContainer() *LogMessage {
	vol := types.Volume{Name: r.Name, Trash: true}
	sourceVol := types.Volume{Name: r.SourceVolumeName, Trash: true}
	if reflect.ValueOf(r.Container).IsNil() {
		containerBuilt, containerErr := r.InitBackupContainer(&vol, &sourceVol)
		if containerErr != nil {
			sentry.CaptureException(containerErr)
			return &LogMessage{Message: containerErr.Error()}
		}
		if !containerBuilt {
			return &LogMessage{Message: "Failed to build backup container"}
		}
	}

	cmd := []string{"borg --log-json"}
	cmd = append(cmd, "--lock-wait "+viper.GetString("backups.borg.lock_wait"))
	cmd = append(cmd, "compact --error --verbose")

	if _, _, log := r.ExecWithLog(cmd); log != (LogMessage{}) {
		return &log
	}

	// Refresh Consul on-disk usage stats now that space has been reclaimed.
	r.Sync()
	borgLogger().Info("Completed compact event", "volume_name", r.Name)
	return nil
}

// compactNFS runs `borg compact` on the NFS/backup server over SSH, then fixes
// ownership of any segments borg rewrote (borg runs as the SSH user there, while
// the container writes as the NFS-squash fs_user/fs_group — a mismatch would make
// the files unreadable on the next backup). Mirrors the host cron it replaces.
// Consul usage stats refresh on the next backup/prune (both call SyncConsul); we
// don't build a container here just to re-read them.
func (r *Repository) compactNFS() *LogMessage {
	cmd, ok := nfsCompactCommand(r.Name)
	if !ok {
		return &LogMessage{Message: "refusing to compact: unsafe repository name " + r.Name}
	}

	connInfo := sshremote.ServerConnInfo{
		Server: viper.GetString("backups.borg.nfs_host"),
		Port:   viper.GetString("backups.borg.nfs_ssh.port"),
		User:   viper.GetString("backups.borg.nfs_ssh.user"),
		Key:    viper.GetString("backups.borg.nfs_ssh.keyfile"),
	}

	if _, err := sshremote.SSHCommandString(cmd, connInfo); err != nil {
		// SSHCommandString folds remote stderr into err; stdout is empty on error.
		borgLogger().Error("NFS compact failed", "volume", r.Name, "error", err.Error())
		sentry.CaptureException(err)
		return &LogMessage{Message: err.Error()}
	}

	borgLogger().Info("Completed compact event", "volume_name", r.Name, "backend", "nfs")
	return nil
}

// nfsCompactCommand builds the remote shell command to compact a repo locally on
// the NFS server, then chown the result to the NFS-squash user (mirrors the host
// cron it replaces). r.Name is interpolated into the command, so an unsafe name
// returns ok=false rather than building it.
func nfsCompactCommand(name string) (string, bool) {
	if !safeRepoName(name) {
		return "", false
	}
	basePath := viper.GetString("backups.borg.nfs_host_path") + "/b-" + name
	cmd := viper.GetString("backups.borg.nfs_borg_path") + " compact --verbose --log-json " + basePath + "/backup" +
		" && chown -R " + viper.GetString("backups.borg.nfs_ssh.fs_user") + ":" +
		viper.GetString("backups.borg.nfs_ssh.fs_group") + " " + basePath
	return cmd, true
}

// Sync reports the repository's observed state (on-disk size + archive names) UP
// into control.db via the changelog (entity_type "repository"). It is the
// store-backed successor to the old Consul borg/repository/<name> write. A no-op
// when there is no store handle or no live container to read from.
func (r *Repository) Sync() {
	if r.Store == nil {
		return
	}
	if reflect.ValueOf(r.Container).IsNil() {
		return
	}

	// get list of archives
	contents, contentsErr := r.Contents()
	if contentsErr != nil {
		borgLogger().Error("Error during Sync", "function", "Repository.Contents()", "errorCode", contentsErr.MsgID, "error", contentsErr.Message)
		return
	}

	archives := []string{}
	for _, item := range contents.Archives {
		archives = append(archives, item.Name)
	}

	repoInfo, repoInfoErr := r.Info()
	if repoInfoErr != nil {
		borgLogger().Error("Error during Sync", "function", "Repository.Info()", "errorCode", repoInfoErr.MsgID, "error", repoInfoErr.Message)
		return
	}

	if err := r.Store.UpsertRepository(context.Background(), store.Repository{
		Name:       r.Name,
		SizeOnDisk: int64(repoInfo.Cache.Stats.UniqueCSize),
		TotalSize:  int64(repoInfo.Cache.Stats.TotalCSize),
		Archives:   archives,
	}); err != nil {
		borgLogger().Error("Failed to sync repository to store", "function", "Sync", "error", err.Error())
		sentry.CaptureException(err)
	}
}

func (r *Repository) repoPath() string {
	if viper.GetBool("backups.borg.ssh.enabled") {
		sshUser := viper.GetString("backups.borg.ssh.user")
		sshHost := viper.GetString("backups.borg.ssh.host")
		sshPort := viper.GetString("backups.borg.ssh.port")
		hostPath := viper.GetString("backups.borg.ssh.host_path")
		fullPath := "ssh://" + sshUser + "@" + sshHost + ":" + sshPort + hostPath + "/b-" + r.Name + "/backup"
		return fullPath
	} else {
		return "/mnt/borg/backup"
	}
}
