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
	"strings"

	"github.com/spf13/viper"

	"github.com/getsentry/sentry-go"
)

// FindRepository opens the repository owned by repoOwner, with target as the volume the
// operation is pointed at — the same volume on a backup, prune or export, a different one
// on a cross-volume restore or archive delete.
//
// Which of the two the returned Repository is NAMED after is the whole point: everything
// downstream of Name addresses the repository (BORG_REPO, the b-<name> cache volume, the
// remote path, the repositories row), and only the /mnt/data mount addresses the target.
// Naming it after the target opened the TARGET's repository on the SSH backend, where a
// restore of volume A into volume B then looked for A's archive in B's repository and
// could never find it. Retention comes from repoOwner for the same reason: a retention
// policy is a property of the repository being pruned.
func FindRepository(st *store.Store, target *types.Volume, repoOwner *types.Volume) (*Repository, *LogMessage) {
	// An absent repository owner means the caller only ever had one volume in mind, so
	// the target is it. Defaulted rather than refused because backup/delete.go passes the
	// task's source_volume through unvalidated and the controller only defaults that
	// parameter — an archive delete that arrives without one works today and must keep
	// working. The warning is here because the alternative is a silent guess.
	if repoOwner.Name == "" {
		borgLogger().Warn("No repository owner supplied, defaulting to the target volume", "volume", target.Name)
		repoOwner = target
	}
	// A missing target has no recovery: it is the /mnt/data mount and the label that says
	// what the container is for, and an empty docker volume name is not a thing to mount.
	if target.Name == "" {
		return nil, &LogMessage{Message: "Missing target volume name"}
	}

	r := Repository{Name: repoOwner.Name, Retention: repoOwner.Retention, Store: st}

	containerBuilt, containerErr := r.InitBackupContainer(target)
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
		stampMissingRepository(err)
		r.StopContainer()
		return nil, err
	}

	if repoResponse == (RepositoryResponse{}) {
		r.StopContainer()
		return nil, &LogMessage{MsgID: MsgIDRepositoryMissing, Message: "Missing Repository"}
	}

	return &r, nil
}

// missingRepositoryMsgID is the verdict FindRepository's callers act on: backup.Perform
// reads it to decide whether to run Repository.Setup (borg init) for a volume that has no
// repository yet. It is the ONLY caller that may — restore reads a repository it does not
// own, so the same verdict there means the source has nothing to restore from.
const missingRepositoryMsgID = "Repository.DoesNotExist"

// The two verdicts a caller outside this package branches on, exported so a call site
// cannot get the spelling wrong. It could, and did: backup.Perform compared against
// "InvalidRepository" while borg emits "Repository.InvalidRepository" (the captured record
// is in failure_record_test.go), so the branch that initializes an SSH repository whose
// directory exists but was never initialized never ran and those backups failed forever.
const (
	MsgIDRepositoryMissing = missingRepositoryMsgID

	// MsgIDRepositoryInvalid means the path holds something that is not a repository —
	// including the empty directory a `mkdir -p` leaves on the SSH backup server ahead of
	// `borg init`. NOT the same fact as MsgIDRepositoryMissing (see
	// looksLikeMissingRepository), which is why both are here.
	MsgIDRepositoryInvalid = "Repository.InvalidRepository"
)

// stampMissingRepository supplies that msgid when borg reported the condition in words
// but the record reached us without one.
//
// It is insurance only. Borg emits the msgid itself, spelled exactly as the callers
// compare it, and 90 days of node journals show borg's own
// "Repository /mnt/borg/backup does not exist." record and never a msgid-less
// substitute. It is here because of what the msgid going missing would cost: a new
// volume would never get a repository, silently and permanently, since every later
// backup fails against an uninitialized one.
//
// Two guards, both load-bearing:
//
//   - Only when MsgID is empty, so borg's own verdict is never overwritten — in
//     particular Repository.InvalidRepository must stay itself, because it means the
//     path holds something that is not a repository.
//   - Only does-not-exist wording (see looksLikeMissingRepository).
//
// Only MsgID is set. The message is left exactly as borg wrote it, since that text is
// what the controller shows.
func stampMissingRepository(failure *LogMessage) {
	if failure == nil || failure.MsgID != "" {
		return
	}
	if looksLikeMissingRepository(failure.Message) {
		failure.MsgID = missingRepositoryMsgID
	}
}

// looksLikeMissingRepository reports whether a message is borg saying the repository is
// not there: "Repository /mnt/borg/backup does not exist.", or its ssh:// form on the
// SSH backend.
//
// Deliberately narrow, because a false positive runs borg init. It must not match
// "/mnt/borg/backup is not a valid repository. Check repo config." — a different
// measured condition (msgid Repository.InvalidRepository) meaning the path has content
// in it — nor the response parsers' own "Empty response from borg while reading
// repository info", which is a fault on this host (an OOM kill, a signal) rather than a
// verdict about the repository. Requiring the "Repository " prefix as well as the
// phrase also keeps "Archive x does not exist" out.
func looksLikeMissingRepository(message string) bool {
	m := strings.ToLower(strings.TrimSpace(message))
	return strings.HasPrefix(m, "repository ") && strings.Contains(m, "does not exist")
}

func (r *Repository) FindArchive(name string) (a *Archive, err *LogMessage) {
	a = &Archive{Name: name, Repository: r}
	// Attempt to load archive. Nil = not exist.
	if _, err = a.Info(); err != nil {
		return nil, err
	}
	return a, nil
}

// Setup initializes r's repository (borg init), building the backup container against
// target if one is not already running. r.Name is the repository being created; target only
// supplies the /mnt/data mount and the label saying what the container is for.
func (r *Repository) Setup(target *types.Volume) *LogMessage {
	if reflect.ValueOf(r.Container).IsNil() {
		containerBuilt, containerErr := r.InitBackupContainer(target)
		if containerErr != nil {
			sentry.CaptureException(containerErr)
			return &LogMessage{Message: containerErr.Error()}
		}
		if !containerBuilt {
			return &LogMessage{Message: "Failed to build backup container"}
		}
	}

	// The SSH backend's repository directory has to exist before `borg init` can write
	// into it, and this is the only place that creates it — Setup is the one operation
	// that is allowed to bring a repository into being. It sits OUTSIDE the block above
	// because the container may already be running (FindRepository built one before it
	// discovered the repository was missing), and the mkdir is still owed in that case.
	if err := r.ensureRemoteRepoPath(); err != nil {
		sentry.CaptureException(err)
		return &LogMessage{Message: err.Error()}
	}

	var backupCmd []string

	backupCmd = append(backupCmd, "borg --log-json")
	backupCmd = append(backupCmd, "--lock-wait "+viper.GetString("backups.borg.lock_wait"))
	backupCmd = append(backupCmd, "init --error --encryption=repokey-blake2")

	res := r.RunBorg("borg init", backupCmd)

	if res.Failure != nil {
		if !repositoryAlreadyExists(res.Failure) {
			return res.Failure
		}
		borgLogger().Info("Repository was already initialized", "volume_name", r.Name, "msgid", res.Failure.MsgID)
	}

	// Register the (now-initialized) repository's observed state in control.db.
	r.Sync()
	return nil
}

// alreadyExistsMsgID is borg's verdict when init found a repository already at the
// path: measured on borg 1.4.4 as exit 2 with this msgid.
const alreadyExistsMsgID = "Repository.AlreadyExists"

// repositoryAlreadyExists reports whether a `borg init` failure means the repository
// the caller asked for is already there — which is exactly what Setup wanted, so it
// counts as success and Setup goes on to Sync.
//
// The tolerance exists because two tasks can genuinely reach init for the same
// never-initialized volume: backup creation deliberately takes no per-repo mutex (see
// AcquireRepoLock) and the dispatcher runs queue.numworkers + 1 backup workers, so a
// second task for the same volume can run concurrently and one of the two loses the
// race. Without this the loser fails a backup that has nothing wrong with it.
//
// Matched on the msgid alone, never on the message text. Repository.PathAlreadyExists
// ("There is already something at /mnt/borg/backup.") is a different measured msgid
// meaning the path holds content that is not a repository; tolerating it would hand
// the caller a phantom repository over a directory with junk in it, and every backup
// against it would fail. Only borg's own verdict is trusted, so a synthesized failure —
// which never carries a msgid — can never be tolerated either.
func repositoryAlreadyExists(failure *LogMessage) bool {
	return failure != nil && failure.MsgID == alreadyExistsMsgID
}

func (r *Repository) Info() (RepositoryResponse, *LogMessage) {
	if reflect.ValueOf(r.Container).IsNil() {
		return RepositoryResponse{}, &LogMessage{Message: "Missing backup container"}
	}

	// --bypass-lock fixes a pre-existing failure rather than trading anything away.
	// backups.borg.lock_wait defaults to 1 second (only lock_wait_create is longer) and
	// `borg create` holds the exclusive lock for the whole backup, while backup creation
	// deliberately does not take the per-repo mutex — so reading a repository while it is
	// being backed up hits a LockTimeout, which is rc 2 with a msgid that readRepoResponse
	// already surfaces today. That is why exporting, deleting or restoring a volume during
	// its own backup fails at this call, one step before Archive.Info.
	//
	// Measured on borg 1.4.4 with `--bypass-lock --lock-wait 1 info --error --json`:
	// healthy repo rc 0; under a held exclusive lock rc 0 with no record (rc 2 with msgid
	// LockTimeout without the bypass); missing repo still rc 2 with msgid
	// Repository.DoesNotExist; an existing non-repo path still rc 2 with msgid
	// Repository.InvalidRepository. So the auto-init signal is undisturbed. `info` is
	// read-only and the callers that go on to mutate take their own lock afterwards.
	//
	// Repository.Contents deliberately does NOT get the bypass: it feeds Sync, and a
	// bypassed read during a concurrent compact (which rewrites segments) could write a
	// torn archive list into control.db.
	//
	// --lock-wait stays, and this ordering — globals before the subcommand — is what was
	// measured working.
	cmd := []string{"borg --log-json --bypass-lock"}
	cmd = append(cmd, "--lock-wait "+viper.GetString("backups.borg.lock_wait"))
	cmd = append(cmd, "info --error --json")

	res := r.RunBorg("borg info", cmd)

	if res.Failure != nil {
		return RepositoryResponse{}, res.Failure
	}

	repoResponse, repoLog := readRepoResponse(res.Response)

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
	return r.TrashBackupVolumeExists()
}

/*
*

		Prune Repository

		*  Will ignore all repositories that don't match the `auto-` prefix.
	    *  Testing this by creating 2 backups back-to-back, and then running prune with
		   an hourly retention of 2 will only retain 1 because the content would not have changed between the 2 backups.
*/
func (r *Repository) Prune() *LogMessage {
	// Trash: true, so the container gets no /mnt/data mount — prune only ever touches the
	// repository. It is the repository's own volume either way: a prune is always run
	// against a volume this node owns.
	vol := types.Volume{Name: r.Name, Trash: true}
	if reflect.ValueOf(r.Container).IsNil() {
		containerBuilt, containerErr := r.InitBackupContainer(&vol)
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

	res := r.RunBorg("borg prune", cmd)

	if res.Failure != nil {
		return res.Failure
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
	// As in Prune: Trash: true means no /mnt/data mount, because a compact rewrites the
	// repository's segments and never reads the volume.
	vol := types.Volume{Name: r.Name, Trash: true}
	if reflect.ValueOf(r.Container).IsNil() {
		containerBuilt, containerErr := r.InitBackupContainer(&vol)
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

	res := r.RunBorg("borg compact", cmd)

	if res.Failure != nil {
		return res.Failure
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

	// A node reports observed state only for the volumes it owns. The row is keyed on
	// r.Name and the changelog entry is ingested by the controller, so syncing a
	// repository that belongs to another node's volume would publish a repositories row
	// this node has no business writing — reachable now that r.Name can be a volume other
	// than the task's own (Archive.Delete calls Sync, and an archive delete can be
	// cross-volume). Every current path — backup, prune, compact, trash — operates on this
	// node's own volumes, so this suppresses nothing that is correct today.
	//
	// Only a definite "not here" skips. A store that could not answer leaves today's
	// behaviour alone rather than dropping a legitimate sync over a transient read error.
	if _, found, err := r.Store.GetVolume(context.Background(), r.Name); err != nil {
		borgLogger().Debug("Could not confirm repository ownership before sync", "repository", r.Name, "error", err.Error())
	} else if !found {
		borgLogger().Debug("Skipping repository sync for a volume this node does not own", "repository", r.Name)
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
	return repoPathFor(r.Name)
}

// repoPathFor is repoPath keyed on a bare name, so containerSpec can build BORG_REPO
// without a *Repository to hang it on — the container spec is the seam the
// repoOwner-vs-target invariant is tested at, and it must be callable without docker.
//
// name is ALWAYS the repository owner. On the SSH backend it is spliced into the remote
// URL, which is exactly where naming the repository after the target volume sent a restore
// to the wrong repository. On local/NFS the docker volume mounted at /mnt/borg IS the
// repository, so the path is a constant and the owner is expressed by which volume got
// mounted there.
func repoPathFor(name string) string {
	if viper.GetBool("backups.borg.ssh.enabled") {
		sshUser := viper.GetString("backups.borg.ssh.user")
		sshHost := viper.GetString("backups.borg.ssh.host")
		sshPort := viper.GetString("backups.borg.ssh.port")
		hostPath := viper.GetString("backups.borg.ssh.host_path")
		fullPath := "ssh://" + sshUser + "@" + sshHost + ":" + sshPort + hostPath + "/b-" + name + "/backup"
		return fullPath
	} else {
		return "/mnt/borg/backup"
	}
}
