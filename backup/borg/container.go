package borg

import (
	"context"
	"cs-agent/containermgr"
	"cs-agent/sshremote"
	"cs-agent/types"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	volumeTypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/spf13/viper"
)

// InitBackupContainer builds the borg container for an operation on target.
//
// It takes no repository-owner parameter: r.Name IS the owner (see Repository.Name), and a
// parameter that must always equal a field is what let the two drift apart in the first
// place — FindRepository recorded the target as the repository's name while passing the
// owner separately, and the SSH backend then built BORG_REPO from the wrong one.
func (r *Repository) InitBackupContainer(target *types.Volume) (bool, error) {
	if !reflect.ValueOf(r.Container).IsNil() {
		// If a container already exists, stop.
		return true, nil
	}
	cli, clientErr := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if clientErr != nil {
		borgLogger().Error("Unable to connect to Docker", "error", clientErr.Error())
		return false, clientErr
	}

	// Check if the borg cache volume exists, and create it if it does not.
	if _, volErr := r.ensureCacheVolume(cli); volErr != nil {
		return false, volErr
	}

	ctx := context.Background()

	// Ensure image exists
	_, _, missingImage := cli.ImageInspectWithRaw(ctx, viper.GetString("backups.borg.image"))
	if missingImage != nil {
		body, pullErr := cli.ImagePull(ctx, viper.GetString("backups.borg.image"), image.PullOptions{})
		if pullErr != nil {
			borgLogger().Error("Fatal error pulling image", "error", pullErr.Error())
			return false, pullErr
		}
		if streamErr := drainImagePull(body); streamErr != nil {
			borgLogger().Error("Fatal error pulling image", "error", streamErr.Error())
			return false, streamErr
		}
	}

	var containerID string
	for attempt := 1; ; attempt++ {
		id, startErr := r.createAndStartContainer(ctx, cli, target)
		if startErr == nil {
			containerID = id
			break
		}
		if attempt >= mountFailureAttempts || !isTransientMountFailure(startErr) {
			return false, startErr
		}
		// Logged, and at WARN, because a retry that works is otherwise invisible: the
		// operation completes, nothing is captured, and the condition that needed two
		// attempts leaves no trace. That matters for a failure the host is expected to
		// stop producing — without this line "the host was fixed" and "the retry is
		// absorbing it" cannot be told apart, and neither can a window that has grown
		// long enough to outlast every attempt.
		borgLogger().Warn("Retrying the backup container after a transient mount failure",
			"repository", r.Name, "attempt", attempt, "attempts", mountFailureAttempts, "error", startErr.Error())
		time.Sleep(mountFailureDelay)
	}

	r.Container = &containermgr.Container{ID: containerID}

	// time.Sleep(250 * time.Millisecond)
	isReady := false
	for counter := 1; counter < 12; counter++ {
		c, errRunning := cli.ContainerInspect(ctx, containerID)
		if errRunning == nil && c.State.Running {
			isReady = true
			break
		} else if errRunning == nil {
			borgLogger().Debug("Waiting for container before executing command", "container", c.ID, "state", c.State.Status)
		} else {
			borgLogger().Debug("Waiting for container before executing command", "error", errRunning.Error())
			// For fatal errors, reduce the total count to 3 instead of 12.
			if counter > 2 {
				break
			}
		}
		time.Sleep(time.Second)
	}
	if !isReady {
		return false, errors.New("container never came online")
	}

	return true, nil
}

// mountFailureAttempts is how many times a backup container is created and started before
// a mount failure is reported, and mountFailureDelay is the wait between attempts.
//
// Sized from the observed condition rather than taste: the kernel refuses the NFS mount in
// windows of roughly three seconds, with mounts succeeding within a second either side, and
// each refusal comes back in well under a second. Two retries five seconds apart therefore
// span a window comfortably longer than any that has been measured. The cost of the wait is
// paid only on a failure, and it is bounded — a backup that is going to fail anyway takes
// ten seconds longer to say so.
const (
	mountFailureAttempts = 3
	mountFailureDelay    = 5 * time.Second
)

// isTransientMountFailure reports whether a container failed to start because the kernel
// could not allocate memory while mounting the repository volume.
//
// It matches on the message text, which is not a preference: the ENOMEM is raised by the
// kernel inside the daemon's own mount call, and by the time it reaches this process it has
// been rendered into the API's error string. There is no errno left for errors.Is to
// compare against.
//
// Deliberately narrow. A gate that matched more would spend mountFailureDelay per volume
// across a whole sweep re-attempting failures a retry cannot fix — a wrong nfs_host, an
// image that is not there, a repository name the server rejects — and would turn a node
// that is simply misconfigured into one that takes minutes to say so. Matching too little
// only means a retryable failure is reported as it is today, which is why this is the safer
// direction to be wrong in.
func isTransientMountFailure(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "cannot allocate memory")
}

// createAndStartContainer creates one attempt's backup container and starts it, returning
// the container's id.
//
// Create and start are ONE unit, and that is the whole reason this is a function. The
// container runs with AutoRemove, and the daemon force-removes an AutoRemove container as
// soon as its start fails — before the start call even returns. So the container a retry
// would want to start again no longer exists: retrying the start alone reports "No such
// container" and replaces the reason the operation actually failed. The NFS mount is
// performed by the daemon inside that same start, which is what makes the pair the smallest
// thing worth re-attempting.
//
// The name is generated per attempt, so two attempts cannot collide on it.
func (r *Repository) createAndStartContainer(ctx context.Context, cli *client.Client, target *types.Volume) (string, error) {
	t := time.Now()
	randNumber := 10 + rand.Intn(1000-10)
	containerName := "backup-" + strconv.Itoa(randNumber) + t.Format("150405")

	labels, borgEnv, mounts := r.containerSpec(target)

	hostConfig := container.HostConfig{
		NetworkMode: "host",
		Binds:       []string{},
		Mounts:      mounts,
		AutoRemove:  true,
		Privileged:  viper.GetBool("docker.privileged"),
	}

	resp, err := cli.ContainerCreate(ctx, &container.Config{
		Image:  viper.GetString("backups.borg.image"),
		Labels: labels,
		Env:    borgEnv,
	}, &hostConfig, nil, nil, containerName)

	if err != nil {
		return "", err
	}

	if startErr := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); startErr != nil {
		// Normally a no-op: the daemon has already removed it. But a removal the daemon
		// could not complete is only LOGGED on its side, so this covers the case where the
		// container is left behind holding the mount it failed to make. The b-<name> cache
		// volume is a named mount and is never removed with a container, so neither the
		// daemon's removal nor this one can take the repository with it.
		if rmErr := cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true}); rmErr != nil && !client.IsErrNotFound(rmErr) {
			borgLogger().Debug("Could not remove a backup container that failed to start", "container", resp.ID, "error", rmErr.Error())
		}
		return "", startErr
	}

	return resp.ID, nil
}

// drainImagePull consumes an image pull to completion and reports the failure it carried.
//
// Two things make this necessary rather than tidy. ImagePull's error covers the REQUEST
// only: the pull itself streams its outcome back in the body, so a pull that could not
// authenticate or could not find the tag returns a nil error and reports it as an `error`
// record partway through the stream. And the body is the caller's to close — discarding it
// leaks the connection and lets the create below race ahead of a pull that has not
// finished, which is the shape of the "wait 1 second to make sure image is available"
// sleep in the mysql strategy.
//
// Reading to EOF is therefore both the close and the wait. The first error record is
// returned, because everything after it describes a pull that is already lost.
func drainImagePull(body io.ReadCloser) error {
	if body == nil {
		return nil
	}
	defer body.Close()

	decoder := json.NewDecoder(body)
	for {
		var record struct {
			Error string `json:"error"`
		}
		if err := decoder.Decode(&record); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if record.Error != "" {
			return errors.New(record.Error)
		}
	}
}

// containerSpec is the decided-by-configuration part of InitBackupContainer: the labels,
// the borg environment and the mounts. No docker client, no image pull, and deliberately
// not the random container name — that reads time.Now() and rand, which would make the
// result unrepeatable and is the one piece of InitBackupContainer that has to stay there.
//
// It is a METHOD taking only the target, so the repository owner cannot be handed to it
// wrongly — there is no argument to get wrong, the same reason InitBackupContainer stopped
// taking one. It is a separate function at all because of the invariant it makes checkable:
//
//   - BORG_REPO and the b-<name> mount at /mnt/borg follow r.Name, the repository owner;
//   - the /mnt/data mount and the com.computestacks.for label follow target.
//
// On every same-volume operation — backup, prune, compact, export — the two are equal and
// nothing tells them apart. Only a cross-volume restore or archive delete does, and neither
// can be exercised without a docker daemon and a backup server, which is exactly why the
// wrong one being used went unnoticed.
//
// b-<r.Name> at /mnt/borg means two different things by backend, and it is the owner's
// volume in both: on local/NFS that docker volume IS the repository, while on SSH it holds
// only borg's BORG_BASE_DIR cache for a repository that lives on the backup server.
//
// A trashed target gets no /mnt/data mount: either its volume is being destroyed, or the
// operation (prune, compact) never reads it.
func (r *Repository) containerSpec(target *types.Volume) (labels map[string]string, env []string, mounts []mount.Mount) {
	labels = map[string]string{
		"com.computestacks.role": "backup",
		"com.computestacks.for":  target.Name,
	}

	if viper.GetBool("backups.borg.ssh.enabled") {
		labels["com.computestacks.backup-kind"] = "ssh"
	} else if viper.GetBool("backups.borg.nfs") {
		labels["com.computestacks.backup-kind"] = "nfs"
	} else {
		labels["com.computestacks.backup-kind"] = "local"
	}

	env = []string{
		"BORG_PASSPHRASE=" + viper.GetString("backups.key"),
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",
		"BORG_DELETE_I_KNOW_WHAT_I_AM_DOING=YES",
		"BORG_CHECK_I_KNOW_WHAT_I_AM_DOING=YES",
		"BORG_BASE_DIR=/mnt/borg",
		"BORG_REPO=" + repoPathFor(r.Name),
	}

	if viper.GetBool("backups.borg.ssh.enabled") {
		env = append(env, "BORG_REMOTE_PATH="+viper.GetString("backups.borg.ssh_borg_remote_path"))
		env = append(env, "BORG_RSH=ssh -i "+viper.GetString("backups.borg.ssh.keyfile"))
	}

	mounts = []mount.Mount{
		{
			Type:     mount.TypeBind,
			Source:   "/etc/computestacks",
			ReadOnly: true,
			Target:   "/etc/computestacks",
		},
		{
			Type:   mount.TypeVolume,
			Source: "b-" + r.Name,
			Target: "/mnt/borg",
		},
	}

	if !target.Trash {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: target.Name,
			Target: "/mnt/data",
		})
	}

	return labels, env, mounts
}

/*
 * Helper methods to deal with situations where the container is null.
 */

// StopContainer will stop the backup container.
func (r *Repository) StopContainer() bool {
	// A nil repository (e.g. FindRepository returned nil on a build/init
	// failure and the deferred cleanup still fires) has nothing to stop.
	if r == nil || r.Container == nil {
		return true
	}
	return r.Container.Stop()
}

/*
Volumes
*/
// ensureCacheVolume creates the docker volume b-<r.Name> if it is not already there.
//
// It takes no volume parameter: the volume is the repository owner's, always, because of
// what is mounted at /mnt/borg — the repository itself on local/NFS, borg's cache for the
// remote repository on SSH. Passing one alongside r.Name is how the local and remote halves
// of this were able to name different volumes.
//
// The NFS remote mkdir/chown STAYS here, unlike the SSH one (see ensureRemoteRepoPath).
// That directory is a precondition of the NFS mount this function is about to declare — the
// volume's driver options point the mount straight at it — so on that backend it belongs to
// volume creation rather than to repository creation, and it is correctly gated on the
// volume's own absence.
func (r *Repository) ensureCacheVolume(cli *client.Client) (bool, error) {
	ctx := context.Background()

	_, existingVolumeErr := cli.VolumeInspect(ctx, "b-"+r.Name)

	if existingVolumeErr != nil {
		// Container Labels
		labels := make(map[string]string)
		labels["com.computestacks.role"] = "backup"
		labels["com.computestacks.for"] = r.Name

		// Driver Opts
		driverOpts := make(map[string]string)

		if viper.GetBool("backups.borg.nfs") {
			if viper.GetBool("backups.borg.nfs_create_path") {
				borgLogger().Info("Creating remote volume directory", "volume", "b-"+r.Name, "type", "nfs")
				sshCmd, ok := nfsRepoPathCommand(r.Name)
				if !ok {
					borgLogger().Error("Refusing to create remote directory with an unsafe repository name", "volume", "b-"+r.Name, "type", "nfs")
					return false, errors.New("refusing to create remote directory: unsafe repository name " + r.Name)
				}
				connInfo := sshremote.ServerConnInfo{
					Server: viper.GetString("backups.borg.nfs_host"),
					Port:   viper.GetString("backups.borg.nfs_ssh.port"),
					User:   viper.GetString("backups.borg.nfs_ssh.user"),
					Key:    viper.GetString("backups.borg.nfs_ssh.keyfile"),
				}

				createDirSuccess, createDirErr := sshremote.SSHCommandBool(sshCmd, connInfo)

				if createDirErr != nil {
					borgLogger().Error("Fatal error creating directory on remote server", "volume", "b-"+r.Name, "error", createDirErr.Error())
					return false, createDirErr
				}

				if !createDirSuccess {
					borgLogger().Warn("Invalid response while creating remote directory", "volume", "b-"+r.Name)
					return false, errors.New("invalid response while creating directory")
				}
			}
			driverOpts["type"] = "nfs"
			driverOpts["o"] = "addr=" + viper.GetString("backups.borg.nfs_host") + ",rw,nfsvers=4" + viper.GetString("backups.borg.nfs_opts")
			driverOpts["device"] = ":" + viper.GetString("backups.borg.nfs_host_path") + "/b-" + r.Name
		}

		opts := volumeTypes.CreateOptions{
			Name:       "b-" + r.Name,
			Driver:     "local",
			DriverOpts: driverOpts,
			Labels:     labels,
		}

		_, volErr := cli.VolumeCreate(ctx, opts)

		if volErr != nil {
			borgLogger().Warn("Fatal Error Creating Volume", "volume", "b-"+r.Name, "error", volErr.Error())
			return false, volErr
		}

		return true, nil

	}
	return true, nil
}

// ensureRemoteRepoPath creates r's repository directory on the SSH backup server.
//
// It is called from Setup and from nowhere else. Creating a repository is what Setup is
// for, and every other operation only READS one: it used to run from inside
// ensureCacheVolume, so a restore, an export, an archive delete or a prune against a
// repository the node does not have would mkdir it on the backup server on the way to
// failing, and a misconfigured node left a trail of empty directories there. Worse, an
// empty directory is not a repository, so the mkdir converted borg's
// Repository.DoesNotExist verdict into Repository.InvalidRepository.
//
// Not gated on the cache volume's absence either, for the reason it cannot be: b-<name>
// exists on a node that has backed the volume up before, while the remote directory may
// have been lost, and Setup is the one caller that needs it there unconditionally.
//
// The !nfs term preserves today's behaviour exactly. ensureCacheVolume tested nfs FIRST, so
// a configuration with both flags set never reached the SSH mkdir, and this must not become
// the change that gives such a node a new remote write.
func (r *Repository) ensureRemoteRepoPath() error {
	if !viper.GetBool("backups.borg.ssh.enabled") || viper.GetBool("backups.borg.nfs") {
		return nil
	}

	borgLogger().Info("Creating remote repository directory", "repository", "b-"+r.Name, "type", "ssh")
	sshCmd, ok := sshRepoPathCommand(r.Name)
	if !ok {
		borgLogger().Error("Refusing to create remote directory with an unsafe repository name", "repository", r.Name, "type", "ssh")
		return errors.New("refusing to create remote directory: unsafe repository name " + r.Name)
	}
	connInfo := sshremote.ServerConnInfo{
		Server: viper.GetString("backups.borg.ssh.host"),
		Port:   viper.GetString("backups.borg.ssh.port"),
		User:   viper.GetString("backups.borg.ssh.user"),
		Key:    viper.GetString("backups.borg.ssh.keyfile"),
	}

	createDirSuccess, createDirErr := sshremote.SSHCommandBool(sshCmd, connInfo)

	if createDirErr != nil {
		borgLogger().Error("Fatal error creating directory on remote server", "repository", r.Name, "error", createDirErr.Error(), "type", "ssh")
		return createDirErr
	}

	if !createDirSuccess {
		borgLogger().Warn("Invalid response while creating remote directory", "repository", r.Name, "type", "ssh")
		return errors.New("invalid response while creating directory")
	}

	return nil
}

// nfsRepoPathCommand builds the remote shell command that creates the repository
// directory on the NFS server and chowns it to the NFS-squash user (the container
// writes as that user, so borg could not otherwise read it back). The name is
// interpolated into the command, so an unsafe name returns ok=false rather than
// building it.
func nfsRepoPathCommand(name string) (string, bool) {
	if !safeRepoName(name) {
		return "", false
	}
	basePath := viper.GetString("backups.borg.nfs_host_path") + "/b-" + name
	cmd := "mkdir -p " + basePath +
		" && chown -R " + viper.GetString("backups.borg.nfs_ssh.fs_user") + ":" +
		viper.GetString("backups.borg.nfs_ssh.fs_group") + " " + basePath
	return cmd, true
}

// sshRepoPathCommand builds the remote shell command that creates the repository
// directory on the SSH backup server. The name is interpolated into the command,
// so an unsafe name returns ok=false rather than building it.
func sshRepoPathCommand(name string) (string, bool) {
	if !safeRepoName(name) {
		return "", false
	}
	cmd := "mkdir -p " + viper.GetString("backups.borg.ssh.host_path") + "/b-" + name + "/backup"
	return cmd, true
}

// TrashBackupVolumeExists destroys r's repository: the local b-<r.Name> docker volume and,
// on a remote backend, the repository directory on the backup server.
//
// It takes no volume parameter. Both halves derive from r.Name, so the volume that gets
// removed locally and the directory that gets removed remotely cannot name different
// repositories — they could, and the local half read the parameter while some of the remote
// logging already read the field.
//
// KNOWN LIMITATION, pre-existing and deliberately not addressed here: the remote half is
// reached only when the LOCAL docker volume was still there. A teardown whose remote rm -rf
// fails (backup server briefly unreachable) has already removed the local volume, so the
// retry takes the "Volume does not exist, skipping..." path above and reports SUCCESS with
// the customer's backup history still on the backup server and nothing recording that it is
// owed. Same shape as the create-side defect ensureRemoteRepoPath fixes — remote state gated
// on an unrelated local volume — and it wants the same treatment, but the trash path's
// idempotency needs its own thinking and this change does not attempt it.
//
// The name is validated FIRST, ahead of the docker VolumeRemove. The remove-command builders
// refuse an unsafe name and that refusal is what this function returns, but it used to be
// reached only after the local volume was already gone — so an unsafe name destroyed the
// local half of the repository and then failed, leaving the remote half behind with nothing
// left to retry against.
func (r *Repository) TrashBackupVolumeExists() (bool, error) {
	if !safeRepoName(r.Name) {
		borgLogger().Error("Refusing to destroy repository with an unsafe name", "repository", r.Name)
		return false, errors.New("refusing to destroy repository: unsafe repository name " + r.Name)
	}
	ctx := context.Background()
	cli, clientErr := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if clientErr != nil {
		borgLogger().Error("Unable to connect to Docker", "error", clientErr.Error())
		return false, clientErr
	}
	existingVolume, existingVolumeErr := cli.VolumeInspect(ctx, "b-"+r.Name)

	if existingVolumeErr != nil {
		borgLogger().Info("Volume does not exist, skipping...")
		return true, nil
	}
	if removeErr := cli.VolumeRemove(ctx, existingVolume.Name, false); removeErr != nil {
		borgLogger().Error("Error Deleting Volume", "error", removeErr.Error())
		return false, removeErr
	}

	if viper.GetBool("backups.borg.nfs") {
		borgLogger().Info("Cleaning remote volume path", "volume", "b-"+r.Name)

		sshCmd, ok := nfsRepoRemoveCommand(r.Name)
		if !ok {
			borgLogger().Error("Refusing to remove remote directory with an unsafe repository name", "volume", "b-"+r.Name)
			return false, errors.New("refusing to remove remote directory: unsafe repository name " + r.Name)
		}
		connInfo := sshremote.ServerConnInfo{
			Server: viper.GetString("backups.borg.nfs_host"),
			Port:   viper.GetString("backups.borg.nfs_ssh.port"),
			User:   viper.GetString("backups.borg.nfs_ssh.user"),
			Key:    viper.GetString("backups.borg.nfs_ssh.keyfile"),
		}

		destroyDirSuccess, destroyDirErr := sshremote.SSHCommandBool(sshCmd, connInfo)

		if destroyDirErr != nil {
			borgLogger().Error("Error removing remote directory", "volume", "b-"+r.Name, "error", destroyDirErr.Error())
			return false, destroyDirErr
		}

		if !destroyDirSuccess {
			borgLogger().Warn("Invalid response while destroying remote directory", "volume", "b-"+r.Name)
			return false, errors.New("invalid response while destroying directory")
		}
	} else if viper.GetBool("backups.borg.ssh.enabled") {

		borgLogger().Info("Cleaning remote volume path", "repository", r.Name, "method", "ssh")

		sshCmd, ok := sshRepoRemoveCommand(r.Name)
		if !ok {
			borgLogger().Error("Refusing to remove remote directory with an unsafe repository name", "repository", r.Name, "type", "ssh")
			return false, errors.New("refusing to remove remote directory: unsafe repository name " + r.Name)
		}
		connInfo := sshremote.ServerConnInfo{
			Server: viper.GetString("backups.borg.ssh.host"),
			Port:   viper.GetString("backups.borg.ssh.port"),
			User:   viper.GetString("backups.borg.ssh.user"),
			Key:    viper.GetString("backups.borg.ssh.keyfile"),
		}

		destroyDirSuccess, destroyDirErr := sshremote.SSHCommandBool(sshCmd, connInfo)

		if destroyDirErr != nil {
			borgLogger().Error("Error removing remote directory", "repository", r.Name, "error", destroyDirErr.Error())
			return false, destroyDirErr
		}

		if !destroyDirSuccess {
			borgLogger().Warn("Invalid response while destroying remote directory", "repository", r.Name, "type", "ssh")
			return false, errors.New("invalid response while destroying directory")
		}
	} else {
		borgLogger().Info("NFS disabled, skipping remote file cleanup.")
	}

	borgLogger().Info("Successfully removed backup volume", "volume", "b-"+r.Name)
	return true, nil
}

// nfsRepoRemoveCommand builds the remote shell command that removes the repository
// directory from the NFS server. The name is interpolated into the command, so an
// unsafe name returns ok=false rather than building it.
func nfsRepoRemoveCommand(name string) (string, bool) {
	if !safeRepoName(name) {
		return "", false
	}
	cmd := "rm -rf " + viper.GetString("backups.borg.nfs_host_path") + "/b-" + name
	return cmd, true
}

// sshRepoRemoveCommand builds the remote shell command that removes the repository
// directory from the SSH backup server. The name is interpolated into the command,
// so an unsafe name returns ok=false rather than building it.
func sshRepoRemoveCommand(name string) (string, bool) {
	if !safeRepoName(name) {
		return "", false
	}
	cmd := "rm -rf " + viper.GetString("backups.borg.ssh.host_path") + "/b-" + name
	return cmd, true
}
