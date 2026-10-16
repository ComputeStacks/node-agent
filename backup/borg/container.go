package borg

import (
	"bufio"
	"context"
	"cs-agent/containermgr"
	"cs-agent/sshremote"
	"cs-agent/types"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	volumeTypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
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

	containerID, startErr := r.startBackupContainer(cli, target)
	if startErr != nil {
		return false, startErr
	}

	r.Container = &containermgr.Container{ID: containerID}

	ctx := context.Background()

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

// startBackupContainer ensures the b-<r.Name> volume, makes sure the borg image is present,
// and creates and starts the backup container, returning its id.
//
// The whole span runs under the repository's volume lifecycle lock (see
// volumeLifecycleLocks). ensureCacheVolume may remove and re-create the volume, and until a
// container that mounts it has STARTED nothing stops another operation of this agent from
// removing it, or from auto-creating it as a plain local volume inside the gap of a
// re-create. Once the start succeeds the volume is in use and docker itself refuses to
// remove it, which is why the lock ends here rather than spanning the readiness wait or the
// container's lifetime. The image pull sits inside the span only because it always has sat
// between the volume and the container; it holds up nothing but another operation on the
// same repository.
//
// Callers may already hold AcquireRepoLock(r.Name); that is the documented lock order
// (repository lock outer, lifecycle lock inner).
func (r *Repository) startBackupContainer(cli *client.Client, target *types.Volume) (string, error) {
	defer acquireVolumeLifecycleLock(r.Name)()

	// Check if the borg cache volume exists, and create it if it does not.
	if _, volErr := r.ensureCacheVolume(cli); volErr != nil {
		return "", volErr
	}

	ctx := context.Background()

	// Ensure image exists
	_, _, missingImage := cli.ImageInspectWithRaw(ctx, viper.GetString("backups.borg.image"))
	if missingImage != nil {
		body, pullErr := cli.ImagePull(ctx, viper.GetString("backups.borg.image"), image.PullOptions{})
		if pullErr != nil {
			borgLogger().Error("Fatal error pulling image", "error", pullErr.Error())
			return "", pullErr
		}
		if streamErr := drainImagePull(body); streamErr != nil {
			borgLogger().Error("Fatal error pulling image", "error", streamErr.Error())
			return "", streamErr
		}
	}

	for attempt := 1; ; attempt++ {
		id, startErr := r.createAndStartContainer(ctx, cli, target)
		if startErr == nil {
			return id, nil
		}
		if attempt >= mountFailureAttempts || !isTransientMountFailure(startErr) {
			return "", startErr
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
// ensureCacheVolume creates the docker volume b-<r.Name> if it is not already there, and on
// the NFS backend brings an existing one's mount options up to date when that is safe.
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
//
// Docker records a volume's driver options when the volume is created and never again, so
// a change to how the repository is mounted (the NFS version, rsize, async…) would
// otherwise reach only volumes created after it. That is what refreshStaleNFSVolume is for.
//
// Callers must hold the repository's volume lifecycle lock (startBackupContainer does).
func (r *Repository) ensureCacheVolume(cli *client.Client) (bool, error) {
	ctx := context.Background()

	existing, existingVolumeErr := cli.VolumeInspect(ctx, "b-"+r.Name)

	if existingVolumeErr != nil {
		return r.createCacheVolume(ctx, cli)
	}

	if err := r.refreshStaleNFSVolume(ctx, cli, existing); err != nil {
		return false, err
	}
	return true, nil
}

// createCacheVolume is ensureCacheVolume's absent path: on the NFS backend it creates the
// repository directory on the server (when nfs_create_path is set) and then declares the
// docker volume pointing at it; on every other backend it creates a plain local volume.
func (r *Repository) createCacheVolume(ctx context.Context, cli *client.Client) (bool, error) {
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
		driverOpts = nfsVolumeDriverOpts(r.Name)
	}

	if volErr := r.createVolume(ctx, cli, driverOpts); volErr != nil {
		return false, volErr
	}
	return true, nil
}

// createVolume is the docker half of creating b-<r.Name>, and nothing else: no remote
// directory, no inspection. It is shared by the first-time create and the NFS re-create so
// the two cannot disagree on labels.
func (r *Repository) createVolume(ctx context.Context, cli *client.Client, driverOpts map[string]string) error {
	opts := volumeTypes.CreateOptions{
		Name:       "b-" + r.Name,
		Driver:     "local",
		DriverOpts: driverOpts,
		Labels: map[string]string{
			"com.computestacks.role": "backup",
			"com.computestacks.for":  r.Name,
		},
	}

	if _, volErr := cli.VolumeCreate(ctx, opts); volErr != nil {
		borgLogger().Warn("Fatal Error Creating Volume", "volume", "b-"+r.Name, "error", volErr.Error())
		return volErr
	}
	return nil
}

// defaultNFSVersion is the NFS version a repository volume is mounted with when
// backups.borg.nfs_version is empty.
//
// 4.2 rather than "4" because the kernel reads nfsvers=4 as NFS 4.0 (minor version 0), and
// a 4.0 client starts a callback service whose buffer allocation on Linux 6.6–6.16 fails
// outright under memory pressure, refusing the mount with "cannot allocate memory". 4.1 and
// later carry callbacks over the session instead and never make that allocation.
const defaultNFSVersion = "4.2"

// nfsVolumeDriverOpts is the docker `local` driver options an NFS repository volume for the
// repository name is declared with: type, device (the repository directory on the server)
// and o (the mount options).
//
// It is the single definition of "what the volume should look like", used both to create
// the volume and to judge whether an existing one is out of date, so the two can never
// disagree about what current means.
//
// nfs_version is not validated beyond being non-empty: a value the kernel does not accept
// is refused at mount time with a message that names it, which is clearer than anything
// this could say. nfs_opts is appended verbatim and is expected to start with a comma.
func nfsVolumeDriverOpts(name string) map[string]string {
	version := strings.TrimSpace(viper.GetString("backups.borg.nfs_version"))
	if version == "" {
		version = defaultNFSVersion
	}
	return map[string]string{
		"type":   "nfs",
		"o":      "addr=" + viper.GetString("backups.borg.nfs_host") + ",rw,nfsvers=" + version + viper.GetString("backups.borg.nfs_opts"),
		"device": ":" + viper.GetString("backups.borg.nfs_host_path") + "/b-" + name,
	}
}

// addrFromMountOpts returns the addr= value from an NFS mount option string, or "" when it
// has none.
func addrFromMountOpts(o string) string {
	for _, opt := range strings.Split(o, ",") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(opt), "addr="); ok {
			return value
		}
	}
	return ""
}

// nfsVolumeState is what classifyNFSVolume concluded about an existing b-<name> volume.
type nfsVolumeState int

const (
	// nfsVolumeNotApplicable: the node is not on the NFS backend, so the volume is not an
	// NFS mount declaration at all — on the local backend it IS the repository — and it is
	// never touched.
	nfsVolumeNotApplicable nfsVolumeState = iota
	// nfsVolumeUpToDate: declared exactly as configuration says it should be.
	nfsVolumeUpToDate
	// nfsVolumeStale: same server, same repository directory, only the mount options
	// differ. Safe to re-create.
	nfsVolumeStale
	// nfsVolumeDiffers: differs in a way that is NOT safe to correct in place (another
	// server, another directory, not an NFS volume at all). Left alone and reported.
	nfsVolumeDiffers
)

// classifyNFSVolume decides whether an existing volume may be re-created with the desired
// driver options (from nfsVolumeDriverOpts).
//
// Narrow on purpose. A volume is stale — re-creatable — only when it is a `local` volume of
// type nfs, its device is exactly the desired device, the addr= in its mount options is
// exactly the configured nfs_host, and its mount options differ. Only then is the
// re-created volume guaranteed to point at the very directory, on the very server, that
// the old one did. Anything else — a changed nfs_host or nfs_host_path, a volume that is
// not NFS-typed, a different driver — would re-point the repository at a location that
// from the node's side is empty, and borg would then initialize a fresh repository there
// and leave the customer's history behind. Those are reported (the returned reason) and
// left exactly as they are.
func classifyNFSVolume(existing volumeTypes.Volume, desired map[string]string, nfsBackend bool) (nfsVolumeState, string) {
	if !nfsBackend {
		return nfsVolumeNotApplicable, ""
	}
	if existing.Driver != "local" {
		return nfsVolumeDiffers, "volume driver is " + existing.Driver + ", not local"
	}
	if existing.Options["type"] != "nfs" {
		return nfsVolumeDiffers, "volume is not an NFS mount"
	}
	if existing.Options["device"] != desired["device"] {
		return nfsVolumeDiffers, "volume points at a different NFS path"
	}
	if addrFromMountOpts(existing.Options["o"]) != addrFromMountOpts(desired["o"]) {
		return nfsVolumeDiffers, "volume points at a different NFS server"
	}
	if existing.Options["o"] == desired["o"] {
		return nfsVolumeUpToDate, ""
	}
	return nfsVolumeStale, ""
}

// postRemoveAction is what refreshStaleNFSVolume does after its remove attempt, decided by
// afterRemoveDecision from a fresh inspect of the volume.
type postRemoveAction int

const (
	// postRemoveCreate: the volume is gone; declare it with the current options.
	postRemoveCreate postRemoveAction = iota
	// postRemoveKeep: the volume is still there, still the same repository on the same
	// server — docker refused the remove because it is in use. Use it as it is.
	postRemoveKeep
	// postRemoveFail: the volume is there but is no longer a declaration of this
	// repository. Something else created it; no container may start against it.
	postRemoveFail
)

// afterRemoveDecision judges the volume as re-inspected after a remove attempt — nil when
// docker reports it does not exist — rather than trusting the remove call's error. The
// error cannot be trusted either way: a remove that failed may have failed because the
// volume vanished under it, and one that succeeded may have been followed by docker
// auto-creating the name as a plain local volume for a container that mounts it. The state
// of the volume is the only thing that says which branch is safe.
func afterRemoveDecision(current *volumeTypes.Volume, desired map[string]string) (postRemoveAction, string) {
	if current == nil {
		return postRemoveCreate, ""
	}
	if current.Driver == "local" && current.Options["type"] == "nfs" && current.Options["device"] == desired["device"] {
		return postRemoveKeep, ""
	}
	return postRemoveFail, "backup volume " + current.Name + " was replaced by a volume that is not this repository's NFS mount"
}

// verifyNFSVolume checks that a volume is declared exactly as desired.
//
// Needed after every re-create because docker's VolumeCreate on a name that already exists
// with the same driver returns the EXISTING volume and ignores the options it was given. A
// create that "succeeded" therefore proves nothing about the options the volume ended up
// with; if anything raced the create, borg would otherwise run against whatever that was.
func verifyNFSVolume(v volumeTypes.Volume, desired map[string]string) error {
	if v.Driver != "local" {
		return errors.New("backup volume " + v.Name + " has driver " + v.Driver + ", not local")
	}
	for _, key := range []string{"type", "device", "o"} {
		if v.Options[key] != desired[key] {
			return errors.New("backup volume " + v.Name + " has " + key + "=" + strconv.Quote(v.Options[key]) + ", wanted " + strconv.Quote(desired[key]))
		}
	}
	return nil
}

// refreshStaleNFSVolume re-creates an existing NFS repository volume whose mount options
// no longer match configuration, when — and only when — that is safe. The backups are not
// touched: the volume is only docker's declaration of how to mount the repository
// directory, and the re-created declaration points at the same directory on the same
// server (see classifyNFSVolume for exactly what "same" requires).
//
// Every way of not re-creating it leaves the existing volume in place and returns nil: it
// still mounts the repository, just with the old options, and the next operation tries
// again. The only error returns are a failed create, and a volume that after the remove is
// not what it must be — in both cases no container is started, so borg never runs against
// a volume that is missing or is not the repository.
//
// The steps, and why each is there:
//
//   - The volume's host mountpoint must provably NOT be mounted. Docker refuses to remove a
//     volume a container references, but a daemon older than v28 then removes the volume's
//     _data directory with RemoveAll, and through a live NFS mount that deletes the
//     repository on the server. If that cannot be determined — the agent cannot see
//     docker's volume directory, say — that counts as mounted.
//   - VolumeRemove(force=false), never force: docker's refusal is the in-use check.
//   - Re-inspect, and let afterRemoveDecision say what the remove actually did.
//   - Create with docker only. The remote mkdir/chown of the first-time create is skipped:
//     the directory demonstrably exists, since the old volume mounted it, and chown -R over
//     a whole repository is slow.
//   - Verify the result with verifyNFSVolume.
//
// Callers must hold the repository's volume lifecycle lock: between the remove and the
// create the volume does not exist, and that gap is what the lock protects.
func (r *Repository) refreshStaleNFSVolume(ctx context.Context, cli *client.Client, existing volumeTypes.Volume) error {
	name := "b-" + r.Name
	desired := nfsVolumeDriverOpts(r.Name)

	state, reason := classifyNFSVolume(existing, desired, viper.GetBool("backups.borg.nfs"))
	switch state {
	case nfsVolumeNotApplicable, nfsVolumeUpToDate:
		return nil
	case nfsVolumeDiffers:
		borgLogger().Warn("Backup volume options differ from configuration; leaving it as is",
			"volume", name, "reason", reason, "o", existing.Options["o"], "device", existing.Options["device"])
		return nil
	}

	oldOpts := existing.Options["o"]

	mounted, mountErr := volumeMountpointMounted(existing.Mountpoint)
	if mountErr != nil || mounted {
		// WARN rather than INFO: a volume the agent cannot re-create stays on the old mount
		// options indefinitely, and the usual reason — a stopped container left behind
		// holding it — is worth someone noticing.
		args := []any{"volume", name, "o", oldOpts}
		if mountErr != nil {
			args = append(args, "error", mountErr.Error())
		}
		borgLogger().Warn("Backup volume is in use; it keeps its old mount options until it is idle", args...)
		return nil
	}

	removeErr := cli.VolumeRemove(ctx, name, false)
	if removeErr != nil && !errdefs.IsConflict(removeErr) && !client.IsErrNotFound(removeErr) {
		borgLogger().Debug("Could not remove backup volume to re-create it", "volume", name, "error", removeErr.Error())
	}

	var current *volumeTypes.Volume
	if v, inspectErr := cli.VolumeInspect(ctx, name); inspectErr == nil {
		current = &v
	} else if !client.IsErrNotFound(inspectErr) {
		borgLogger().Error("Could not inspect backup volume after removing it", "volume", name, "error", inspectErr.Error())
		return inspectErr
	}

	action, failReason := afterRemoveDecision(current, desired)
	switch action {
	case postRemoveKeep:
		if current.Options["o"] != desired["o"] {
			args := []any{"volume", name, "o", current.Options["o"]}
			if removeErr != nil {
				args = append(args, "error", removeErr.Error())
			}
			borgLogger().Warn("Backup volume is in use; it keeps its old mount options until it is idle", args...)
		}
		return nil
	case postRemoveFail:
		borgLogger().Error("Backup volume changed while being re-created", "volume", name, "reason", failReason)
		return errors.New(failReason)
	}

	if err := r.createAndVerifyNFSVolume(ctx, cli, desired); err != nil {
		return err
	}
	borgLogger().Info("Re-created backup volume with current mount options", "volume", name, "old_o", oldOpts, "new_o", desired["o"])
	return nil
}

// createAndVerifyNFSVolume declares b-<r.Name> with the desired NFS driver options (docker
// only, no remote directory) and then confirms, by inspecting it, that the volume docker
// now has is that declaration — see verifyNFSVolume for why the create's own success is
// not enough.
func (r *Repository) createAndVerifyNFSVolume(ctx context.Context, cli *client.Client, desired map[string]string) error {
	name := "b-" + r.Name
	if err := r.createVolume(ctx, cli, desired); err != nil {
		return err
	}
	created, inspectErr := cli.VolumeInspect(ctx, name)
	if inspectErr != nil {
		borgLogger().Error("Could not inspect re-created backup volume", "volume", name, "error", inspectErr.Error())
		return inspectErr
	}
	if err := verifyNFSVolume(created, desired); err != nil {
		borgLogger().Error("Re-created backup volume does not have the expected options", "volume", name, "error", err.Error())
		return err
	}
	return nil
}

// volumeMountpointMounted reports whether a docker volume's host mountpoint (the _data
// directory VolumeInspect reports) is currently a mount point.
//
// It errors — which callers treat as "mounted" — whenever the answer cannot be trusted:
// no mountpoint reported, or the volume's directory not visible from here (docker running
// with a data root this process cannot see, in another mount namespace, or the agent not
// running as root). "Not in mountinfo" only means "not mounted" when the path it would be
// mounted on is demonstrably one this process can see.
//
// The parent directory is resolved through symlinks first because mountinfo records the
// resolved path, and a data root reached through a symlink would otherwise never match.
func volumeMountpointMounted(mountpoint string) (bool, error) {
	if mountpoint == "" {
		return false, errors.New("volume has no mountpoint")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(filepath.Clean(mountpoint)))
	if err != nil {
		return false, err
	}
	return isMountpoint(filepath.Join(parent, filepath.Base(mountpoint)))
}

// isMountpoint reports whether path is a mount point in this process's mount namespace,
// read from /proc/self/mountinfo. path is compared as given (cleaned), so a caller with a
// path that may run through a symlink must resolve it first.
func isMountpoint(path string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()
	return mountinfoHasMountpoint(f, filepath.Clean(path))
}

// mountinfoHasMountpoint scans mountinfo-formatted text for a mount point equal to path.
// The mount point is the fifth field; the kernel escapes space, tab, newline and backslash
// in it as three-digit octal (\040 and so on), which is undone before comparing.
func mountinfoHasMountpoint(r io.Reader, path string) (bool, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		if unescapeMountinfo(fields[4]) == path {
			return true, nil
		}
	}
	return false, scanner.Err()
}

// unescapeMountinfo undoes the kernel's octal escaping of a mountinfo path field.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
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
//
// The inspect, the remove and the remote delete all run under the repository's volume
// lifecycle lock, the one InitBackupContainer holds while it may be re-creating the volume.
// Without it a trash could inspect inside a re-create's remove→create gap, see no volume,
// skip the remote delete and report success, and the re-create would then bring the volume
// back pointing at the repository that was supposed to be destroyed.
func (r *Repository) TrashBackupVolumeExists() (bool, error) {
	if !safeRepoName(r.Name) {
		borgLogger().Error("Refusing to destroy repository with an unsafe name", "repository", r.Name)
		return false, errors.New("refusing to destroy repository: unsafe repository name " + r.Name)
	}
	defer acquireVolumeLifecycleLock(r.Name)()
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
