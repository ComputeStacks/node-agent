package borg

import (
	"cs-agent/types"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/mount"
	volumeTypes "github.com/docker/docker/api/types/volume"
	"github.com/spf13/viper"
)

// sshBackendConfig is the SSH backend as a node running it is configured: every key
// containerSpec and repoPathFor read, and nothing else, so a value appearing in an
// assertion below can only have come from the key it was set on.
func sshBackendConfig() {
	viper.Reset()
	viper.Set("backups.key", "passphrase")
	viper.Set("backups.borg.ssh.enabled", true)
	viper.Set("backups.borg.ssh.user", "borg")
	viper.Set("backups.borg.ssh.host", "backup.example.com")
	viper.Set("backups.borg.ssh.port", "2222")
	viper.Set("backups.borg.ssh.host_path", "/backups/node001")
	viper.Set("backups.borg.ssh.keyfile", "/etc/computestacks/borg.key")
	viper.Set("backups.borg.ssh_borg_remote_path", "/usr/local/bin/borg")
}

// borgEnvValue reads one variable out of a containerSpec environment. The second return
// is whether it was set at all, because BORG_REMOTE_PATH and BORG_RSH are present on the
// SSH backend only.
func borgEnvValue(env []string, name string) (string, bool) {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, name+"="); ok {
			return value, true
		}
	}
	return "", false
}

// mountSource reads the source of the mount at a container path, so an assertion names
// the path it cares about rather than an index into the slice.
func mountSource(mounts []mount.Mount, target string) (string, bool) {
	for _, m := range mounts {
		if m.Target == target {
			return m.Source, true
		}
	}
	return "", false
}

// TestContainerSpecSSHFollowsTheRepositoryOwner is the regression guard for the defect
// this seam exists for: on the SSH backend BORG_REPO is spliced out of a volume name, and
// building it from the volume the operation is POINTED AT sent a restore of vol-source
// into vol-target looking for vol-source's archive in vol-target's repository. Only a
// cross-volume operation tells the two apart, so target and owner differ here.
//
// Both halves are asserted together on purpose: the repository (BORG_REPO, /mnt/borg) is
// the owner's, and the data (/mnt/data, the com.computestacks.for label) is the target's.
// Asserting either alone would pass with the two swapped.
func TestContainerSpecSSHFollowsTheRepositoryOwner(t *testing.T) {
	sshBackendConfig()

	target := &types.Volume{Name: "vol-target"}
	labels, env, mounts := (&Repository{Name: "vol-owner"}).containerSpec(target)

	wantRepo := "ssh://borg@backup.example.com:2222/backups/node001/b-vol-owner/backup"
	gotRepo, ok := borgEnvValue(env, "BORG_REPO")
	if !ok {
		t.Fatal("Received no BORG_REPO, wanted the repository owner's remote path")
	}
	if gotRepo != wantRepo {
		t.Errorf("Received BORG_REPO %q, wanted %q", gotRepo, wantRepo)
	}
	if strings.Contains(gotRepo, target.Name) {
		t.Errorf("Received BORG_REPO %q naming the target volume %q, wanted the repository owner's", gotRepo, target.Name)
	}

	if got, ok := mountSource(mounts, "/mnt/borg"); !ok || got != "b-vol-owner" {
		t.Errorf("Received /mnt/borg from %q (present=%v), wanted %q", got, ok, "b-vol-owner")
	}
	if got, ok := mountSource(mounts, "/mnt/data"); !ok || got != target.Name {
		t.Errorf("Received /mnt/data from %q (present=%v), wanted %q", got, ok, target.Name)
	}
	if got := labels["com.computestacks.for"]; got != target.Name {
		t.Errorf("Received com.computestacks.for %q, wanted the target %q", got, target.Name)
	}
	if got := labels["com.computestacks.backup-kind"]; got != "ssh" {
		t.Errorf("Received com.computestacks.backup-kind %q, wanted %q", got, "ssh")
	}
}

// TestContainerSpecLocalBackend pins the local backend: the docker volume mounted at
// /mnt/borg IS the repository, so BORG_REPO is a constant and the owner is expressed by
// which volume got mounted there.
func TestContainerSpecLocalBackend(t *testing.T) {
	viper.Reset()
	viper.Set("backups.key", "passphrase")

	labels, env, mounts := (&Repository{Name: "vol-1"}).containerSpec(&types.Volume{Name: "vol-1"})

	if got, ok := borgEnvValue(env, "BORG_REPO"); !ok || got != "/mnt/borg/backup" {
		t.Errorf("Received BORG_REPO %q (present=%v), wanted %q", got, ok, "/mnt/borg/backup")
	}
	if got := labels["com.computestacks.backup-kind"]; got != "local" {
		t.Errorf("Received com.computestacks.backup-kind %q, wanted %q", got, "local")
	}
	if got := labels["com.computestacks.role"]; got != "backup" {
		t.Errorf("Received com.computestacks.role %q, wanted %q", got, "backup")
	}
	if got, ok := mountSource(mounts, "/mnt/borg"); !ok || got != "b-vol-1" {
		t.Errorf("Received /mnt/borg from %q (present=%v), wanted %q", got, ok, "b-vol-1")
	}
	if got, ok := mountSource(mounts, "/mnt/data"); !ok || got != "vol-1" {
		t.Errorf("Received /mnt/data from %q (present=%v), wanted %q", got, ok, "vol-1")
	}
}

// TestContainerSpecNFSBackend covers the third backend's label. BORG_REPO is the same
// constant as local — the NFS-ness lives in the volume's driver options, not the path.
func TestContainerSpecNFSBackend(t *testing.T) {
	viper.Reset()
	viper.Set("backups.key", "passphrase")
	viper.Set("backups.borg.nfs", true)

	labels, env, _ := (&Repository{Name: "vol-1"}).containerSpec(&types.Volume{Name: "vol-1"})

	if got := labels["com.computestacks.backup-kind"]; got != "nfs" {
		t.Errorf("Received com.computestacks.backup-kind %q, wanted %q", got, "nfs")
	}
	if got, ok := borgEnvValue(env, "BORG_REPO"); !ok || got != "/mnt/borg/backup" {
		t.Errorf("Received BORG_REPO %q (present=%v), wanted %q", got, ok, "/mnt/borg/backup")
	}
}

// TestContainerSpecTrashedTargetHasNoDataMount covers prune, compact and a volume being
// destroyed: there is nothing to read at /mnt/data, and mounting a volume that is going
// away would keep it alive. The repository mount is still owed in every one of those
// cases, which is the half that must not be dropped with it.
func TestContainerSpecTrashedTargetHasNoDataMount(t *testing.T) {
	for _, i := range []struct {
		name  string
		setup func()
	}{
		{name: "local", setup: func() { viper.Reset() }},
		{name: "ssh", setup: sshBackendConfig},
	} {
		t.Run(i.name, func(t *testing.T) {
			i.setup()

			_, _, mounts := (&Repository{Name: "vol-1"}).containerSpec(&types.Volume{Name: "vol-1", Trash: true})

			if got, ok := mountSource(mounts, "/mnt/data"); ok {
				t.Errorf("Received a /mnt/data mount from %q, wanted none for a trashed target", got)
			}
			if got, ok := mountSource(mounts, "/mnt/borg"); !ok || got != "b-vol-1" {
				t.Errorf("Received /mnt/borg from %q (present=%v), wanted %q", got, ok, "b-vol-1")
			}
		})
	}
}

// TestContainerSpecRemoteEnv pins the two variables that only mean something when the
// repository is on a backup server: borg's path at the far end of the ssh connection and
// the ssh command that gets there. On the local backend there is no far end.
func TestContainerSpecRemoteEnv(t *testing.T) {
	sshBackendConfig()

	_, env, _ := (&Repository{Name: "vol-1"}).containerSpec(&types.Volume{Name: "vol-1"})

	if got, ok := borgEnvValue(env, "BORG_REMOTE_PATH"); !ok || got != "/usr/local/bin/borg" {
		t.Errorf("Received BORG_REMOTE_PATH %q (present=%v), wanted %q", got, ok, "/usr/local/bin/borg")
	}
	if got, ok := borgEnvValue(env, "BORG_RSH"); !ok || got != "ssh -i /etc/computestacks/borg.key" {
		t.Errorf("Received BORG_RSH %q (present=%v), wanted %q", got, ok, "ssh -i /etc/computestacks/borg.key")
	}

	viper.Reset()
	_, localEnv, _ := (&Repository{Name: "vol-1"}).containerSpec(&types.Volume{Name: "vol-1"})

	for _, name := range []string{"BORG_REMOTE_PATH", "BORG_RSH"} {
		if got, ok := borgEnvValue(localEnv, name); ok {
			t.Errorf("Received %s=%q on the local backend, wanted it unset", name, got)
		}
	}
}

func TestNFSRepoPathCommand(t *testing.T) {
	viper.Reset()
	viper.Set("backups.borg.nfs_host_path", "/mnt/ams001/node001")
	viper.Set("backups.borg.nfs_ssh.fs_user", "nobody")
	viper.Set("backups.borg.nfs_ssh.fs_group", "nogroup")

	cmd, ok := nfsRepoPathCommand("vol-1")
	if !ok {
		t.Fatal("expected ok for a valid repository name")
	}
	want := "mkdir -p /mnt/ams001/node001/b-vol-1" +
		" && chown -R nobody:nogroup /mnt/ams001/node001/b-vol-1"
	if cmd != want {
		t.Errorf("nfsRepoPathCommand mismatch:\n got: %s\nwant: %s", cmd, want)
	}

	if _, ok := nfsRepoPathCommand("bad;name"); ok {
		t.Error("expected an unsafe repository name to be rejected")
	}
}

func TestSSHRepoPathCommand(t *testing.T) {
	viper.Reset()
	viper.Set("backups.borg.ssh.host_path", "/backups/node001")

	cmd, ok := sshRepoPathCommand("vol-1")
	if !ok {
		t.Fatal("expected ok for a valid repository name")
	}
	want := "mkdir -p /backups/node001/b-vol-1/backup"
	if cmd != want {
		t.Errorf("sshRepoPathCommand mismatch:\n got: %s\nwant: %s", cmd, want)
	}

	if _, ok := sshRepoPathCommand("bad;name"); ok {
		t.Error("expected an unsafe repository name to be rejected")
	}
}

func TestNFSRepoRemoveCommand(t *testing.T) {
	viper.Reset()
	viper.Set("backups.borg.nfs_host_path", "/mnt/ams001/node001")

	cmd, ok := nfsRepoRemoveCommand("vol-1")
	if !ok {
		t.Fatal("expected ok for a valid repository name")
	}
	want := "rm -rf /mnt/ams001/node001/b-vol-1"
	if cmd != want {
		t.Errorf("nfsRepoRemoveCommand mismatch:\n got: %s\nwant: %s", cmd, want)
	}

	if _, ok := nfsRepoRemoveCommand("bad;name"); ok {
		t.Error("expected an unsafe repository name to be rejected")
	}
}

func TestSSHRepoRemoveCommand(t *testing.T) {
	viper.Reset()
	viper.Set("backups.borg.ssh.host_path", "/backups/node001")

	cmd, ok := sshRepoRemoveCommand("vol-1")
	if !ok {
		t.Fatal("expected ok for a valid repository name")
	}
	want := "rm -rf /backups/node001/b-vol-1"
	if cmd != want {
		t.Errorf("sshRepoRemoveCommand mismatch:\n got: %s\nwant: %s", cmd, want)
	}

	if _, ok := sshRepoRemoveCommand("bad;name"); ok {
		t.Error("expected an unsafe repository name to be rejected")
	}
}

// TestDrainImagePullReportsTheStreamsOwnError is the regression guard for a pull whose
// failure never reaches the caller. ImagePull returns a nil error for a pull that the
// daemon then abandons — the reason arrives as an `error` record inside the body — so a
// caller that only checks the returned error treats "manifest not found" as a successful
// pull and fails microseconds later on a create that cannot find the image.
//
// The records below are the shape docker streams: newline-delimited JSON objects, progress
// first, and the error (when there is one) partway through rather than at the end.
func TestDrainImagePullReportsTheStreamsOwnError(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		want   string // "" means the pull is to be reported as having succeeded
	}{
		{
			name:   "a pull that completed carries no error",
			stream: `{"status":"Pulling from computestacks/cs-docker-borg","id":"1.6"}` + "\n" + `{"status":"Digest: sha256:abc"}` + "\n" + `{"status":"Status: Downloaded newer image"}`,
		},
		{
			name:   "an empty stream is not a failure",
			stream: "",
		},
		{
			name:   "the error record is reported",
			stream: `{"status":"Pulling from computestacks/cs-docker-borg","id":"1.6"}` + "\n" + `{"errorDetail":{"message":"manifest for cs-docker-borg:1.6 not found"},"error":"manifest for cs-docker-borg:1.6 not found"}`,
			want:   "manifest for cs-docker-borg:1.6 not found",
		},
		{
			name:   "a truncated stream is reported rather than read as success",
			stream: `{"status":"Pulling from computestacks/cs-docker-borg","id":"1.6"}` + "\n" + `{"status":"Downloa`,
			want:   "unexpected EOF",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := drainImagePull(io.NopCloser(strings.NewReader(tc.stream)))

			if tc.want == "" {
				if err != nil {
					t.Fatalf("drainImagePull reported %q for a pull that carried no error", err.Error())
				}
				return
			}
			if err == nil {
				t.Fatalf("drainImagePull reported success for a stream carrying %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("drainImagePull reported %q, want it to carry %q", err.Error(), tc.want)
			}
		})
	}
}

// TestDrainImagePullClosesTheBody pins the close, which is the other half of why this
// function exists: the body is the caller's to close, and a pull whose body is discarded
// leaks the connection. Reading to EOF is also what makes the create that follows wait for
// a pull that has not finished.
func TestDrainImagePullClosesTheBody(t *testing.T) {
	body := &closeRecorder{Reader: strings.NewReader(`{"status":"Status: Downloaded newer image"}`)}

	if err := drainImagePull(body); err != nil {
		t.Fatalf("drainImagePull reported %q for a pull that carried no error", err.Error())
	}
	if !body.closed {
		t.Fatal("drainImagePull returned without closing the body")
	}
}

// closeRecorder is a body that remembers whether it was closed.
type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error {
	c.closed = true
	return nil
}

// TestIsTransientMountFailure pins the gate that decides whether a backup container's
// failure to start is worth re-attempting. The strings are what the docker API actually
// returns — the first is a captured production failure, kernel ENOMEM on the NFSv4 mount of
// a repository volume — because the ENOMEM has no errno left by the time it arrives here
// and the only thing available to match on is this text.
//
// The non-matching cases are the point of the test as much as the matching one: each is a
// failure a retry cannot fix, and each would otherwise cost the retry delay on every volume
// of a sweep before reporting a condition that was true immediately.
func TestIsTransientMountFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "kernel ENOMEM mounting the repository volume",
			err:  errors.New(`error while mounting volume '/var/lib/docker/volumes/b-26a53da6-8b3c-4105-8f96-1ac1e1b246f3/_data': failed to mount local volume: mount :/mnt/backup005/node005/b-26a53da6-8b3c-4105-8f96-1ac1e1b246f3:/var/lib/docker/volumes/b-26a53da6-8b3c-4105-8f96-1ac1e1b246f3/_data, flags: 0x400, data: addr=172.16.2.2,nfsvers=4,rsize=32768,wsize=32768: cannot allocate memory`),
			want: true,
		},
		{
			name: "an unreachable backup server is not retried",
			err:  errors.New(`error while mounting volume: failed to mount local volume: mount :/mnt/backups/b-vol:/var/lib/docker/volumes/b-vol/_data: connection timed out`),
		},
		{
			name: "a missing image is not retried",
			err:  errors.New("Error response from daemon: No such image: ghcr.io/computestacks/cs-docker-borg:1.6"),
		},
		{
			name: "a container that vanished is not retried",
			err:  errors.New("Error response from daemon: No such container: 4f1a"),
		},
		{
			name: "no error is not a failure",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientMountFailure(tc.err); got != tc.want {
				t.Fatalf("isTransientMountFailure = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestMountRetryOutlastsTheObservedWindow guards the sizing of the retry against the
// condition it exists for. The kernel refused the mount in windows of roughly three
// seconds; if the attempts and the delay are ever retuned so that they span less than
// that, the retry stops covering the thing it was added for and every failure it was
// meant to absorb comes back.
func TestMountRetryOutlastsTheObservedWindow(t *testing.T) {
	const observedWindow = 3 * time.Second

	spanned := time.Duration(mountFailureAttempts-1) * mountFailureDelay
	if spanned <= observedWindow {
		t.Fatalf("%d attempts %s apart span %s, which does not outlast the observed %s failure window",
			mountFailureAttempts, mountFailureDelay, spanned, observedWindow)
	}
}

// nfsBackendConfig is the NFS backend as a node running it is configured, with the keys
// nfsVolumeDriverOpts reads. nfs_version is left unset unless a test sets it, so the
// fallback is what gets exercised by default.
func nfsBackendConfig() {
	viper.Reset()
	viper.Set("backups.borg.nfs", true)
	viper.Set("backups.borg.nfs_host", "10.1.2.3")
	viper.Set("backups.borg.nfs_host_path", "/var/nfsshare/volume_backups")
	viper.Set("backups.borg.nfs_opts", ",async,noatime,rsize=32768,wsize=32768")
}

// The version default is the point of the change: nfsvers=4 is NFS 4.0, so the rendered
// option must name 4.2 and must not be the bare "4" it replaces.
func TestNFSVolumeDriverOpts(t *testing.T) {
	cases := []struct {
		name    string
		version any // nil: unset
		wantO   string
	}{
		{name: "unset falls back to 4.2", version: nil, wantO: "addr=10.1.2.3,rw,nfsvers=4.2,async,noatime,rsize=32768,wsize=32768"},
		{name: "empty falls back to 4.2", version: "", wantO: "addr=10.1.2.3,rw,nfsvers=4.2,async,noatime,rsize=32768,wsize=32768"},
		{name: "blank falls back to 4.2", version: "  ", wantO: "addr=10.1.2.3,rw,nfsvers=4.2,async,noatime,rsize=32768,wsize=32768"},
		{name: "custom version honored", version: "4.1", wantO: "addr=10.1.2.3,rw,nfsvers=4.1,async,noatime,rsize=32768,wsize=32768"},
		{name: "4.0 can be restored", version: "4", wantO: "addr=10.1.2.3,rw,nfsvers=4,async,noatime,rsize=32768,wsize=32768"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nfsBackendConfig()
			if tc.version != nil {
				viper.Set("backups.borg.nfs_version", tc.version)
			}
			got := nfsVolumeDriverOpts("vol-a")
			if got["o"] != tc.wantO {
				t.Errorf("Received o %q, wanted %q", got["o"], tc.wantO)
			}
			if got["type"] != "nfs" {
				t.Errorf("Received type %q, wanted nfs", got["type"])
			}
			if want := ":/var/nfsshare/volume_backups/b-vol-a"; got["device"] != want {
				t.Errorf("Received device %q, wanted %q", got["device"], want)
			}
			if len(got) != 3 {
				t.Errorf("Received %d driver options, wanted exactly type, o and device: %v", len(got), got)
			}
		})
	}

	nfsBackendConfig()
	if o := nfsVolumeDriverOpts("vol-a")["o"]; strings.Contains(o, "nfsvers=4,") {
		t.Errorf("Received default o %q, which still selects NFS 4.0", o)
	}
}

func TestAddrFromMountOpts(t *testing.T) {
	cases := map[string]string{
		"addr=10.1.2.3,rw,nfsvers=4":          "10.1.2.3",
		"rw,addr=nfs.example.com,nfsvers=4.2": "nfs.example.com",
		"rw,nfsvers=4.2,addr=10.0.0.1":        "10.0.0.1",
		"rw,nfsvers=4.2":                      "",
		"":                                    "",
	}
	for o, want := range cases {
		if got := addrFromMountOpts(o); got != want {
			t.Errorf("addrFromMountOpts(%q) = %q, wanted %q", o, got, want)
		}
	}
}

// nfsVolume builds an inspected volume as docker reports one declared with opts.
func nfsVolume(driver string, opts map[string]string) volumeTypes.Volume {
	return volumeTypes.Volume{Name: "b-vol-a", Driver: driver, Options: opts}
}

// classifyNFSVolume may call a volume stale ONLY when re-creating it points at the same
// directory on the same server. Every other difference must come back as "differs", never
// as "stale": re-creating those would re-point the repository at an empty location.
func TestClassifyNFSVolume(t *testing.T) {
	nfsBackendConfig()
	desired := nfsVolumeDriverOpts("vol-a")
	oldO := "addr=10.1.2.3,rw,nfsvers=4,async,noatime,rsize=32768,wsize=32768"

	cases := []struct {
		name       string
		volume     volumeTypes.Volume
		nfsBackend bool
		want       nfsVolumeState
	}{
		{"up to date", nfsVolume("local", map[string]string{"type": "nfs", "device": desired["device"], "o": desired["o"]}), true, nfsVolumeUpToDate},
		{"only o differs (nfsvers=4 → 4.2)", nfsVolume("local", map[string]string{"type": "nfs", "device": desired["device"], "o": oldO}), true, nfsVolumeStale},
		{"device differs", nfsVolume("local", map[string]string{"type": "nfs", "device": ":/elsewhere/b-vol-a", "o": oldO}), true, nfsVolumeDiffers},
		{"addr differs", nfsVolume("local", map[string]string{"type": "nfs", "device": desired["device"], "o": "addr=10.9.9.9,rw,nfsvers=4"}), true, nfsVolumeDiffers},
		{"addr missing", nfsVolume("local", map[string]string{"type": "nfs", "device": desired["device"], "o": "rw,nfsvers=4"}), true, nfsVolumeDiffers},
		{"type missing", nfsVolume("local", map[string]string{"device": desired["device"], "o": oldO}), true, nfsVolumeDiffers},
		{"type not nfs", nfsVolume("local", map[string]string{"type": "tmpfs", "device": desired["device"], "o": oldO}), true, nfsVolumeDiffers},
		{"driver not local", nfsVolume("other", map[string]string{"type": "nfs", "device": desired["device"], "o": oldO}), true, nfsVolumeDiffers},
		{"options nil", nfsVolume("local", nil), true, nfsVolumeDiffers},
		{"not the nfs backend", nfsVolume("local", map[string]string{"type": "nfs", "device": desired["device"], "o": oldO}), false, nfsVolumeNotApplicable},
		{"not the nfs backend, plain volume", nfsVolume("local", nil), false, nfsVolumeNotApplicable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := classifyNFSVolume(tc.volume, desired, tc.nfsBackend)
			if got != tc.want {
				t.Errorf("Received state %d, wanted %d (reason %q)", got, tc.want, reason)
			}
			if (got == nfsVolumeDiffers) != (reason != "") {
				t.Errorf("Received reason %q for state %d; a reason belongs to, and only to, the differs state", reason, got)
			}
		})
	}
}

func TestAfterRemoveDecision(t *testing.T) {
	nfsBackendConfig()
	desired := nfsVolumeDriverOpts("vol-a")

	if got, _ := afterRemoveDecision(nil, desired); got != postRemoveCreate {
		t.Errorf("absent volume: received %d, wanted create", got)
	}

	busy := nfsVolume("local", map[string]string{"type": "nfs", "device": desired["device"], "o": "addr=10.1.2.3,rw,nfsvers=4"})
	if got, _ := afterRemoveDecision(&busy, desired); got != postRemoveKeep {
		t.Errorf("same repository still present: received %d, wanted keep", got)
	}

	for name, v := range map[string]volumeTypes.Volume{
		"plain local (docker auto-create)": nfsVolume("local", nil),
		"different device":                 nfsVolume("local", map[string]string{"type": "nfs", "device": ":/elsewhere", "o": desired["o"]}),
		"different driver":                 nfsVolume("other", map[string]string{"type": "nfs", "device": desired["device"], "o": desired["o"]}),
	} {
		v := v
		got, reason := afterRemoveDecision(&v, desired)
		if got != postRemoveFail || reason == "" {
			t.Errorf("%s: received %d (%q), wanted fail with a reason", name, got, reason)
		}
	}
}

func TestVerifyNFSVolume(t *testing.T) {
	nfsBackendConfig()
	desired := nfsVolumeDriverOpts("vol-a")

	if err := verifyNFSVolume(nfsVolume("local", map[string]string{"type": "nfs", "device": desired["device"], "o": desired["o"]}), desired); err != nil {
		t.Errorf("exact match: received %v, wanted nil", err)
	}
	for name, v := range map[string]volumeTypes.Volume{
		"plain local":      nfsVolume("local", nil),
		"old o":            nfsVolume("local", map[string]string{"type": "nfs", "device": desired["device"], "o": "addr=10.1.2.3,rw,nfsvers=4"}),
		"different device": nfsVolume("local", map[string]string{"type": "nfs", "device": ":/elsewhere", "o": desired["o"]}),
		"different driver": nfsVolume("other", map[string]string{"type": "nfs", "device": desired["device"], "o": desired["o"]}),
	} {
		if err := verifyNFSVolume(v, desired); err == nil {
			t.Errorf("%s: received nil, wanted an error", name)
		}
	}
}

func TestIsMountpoint(t *testing.T) {
	if got, err := isMountpoint("/"); err != nil || !got {
		t.Errorf("isMountpoint(/) = %v, %v; wanted true, nil", got, err)
	}
	dir := t.TempDir()
	if got, err := isMountpoint(dir); err != nil || got {
		t.Errorf("isMountpoint(%s) = %v, %v; wanted false, nil", dir, got, err)
	}
}

// The mountinfo mount point field is octal-escaped; a docker data root with a space in it
// must still be recognized.
func TestMountinfoHasMountpointUnescapes(t *testing.T) {
	info := "36 35 98:0 / /mnt/data\\040root/volumes/b-x/_data rw,noatime master:1 - nfs4 10.1.2.3:/b-x rw\n" +
		"37 35 98:0 / /other rw - ext4 /dev/sda1 rw\n"
	got, err := mountinfoHasMountpoint(strings.NewReader(info), "/mnt/data root/volumes/b-x/_data")
	if err != nil || !got {
		t.Errorf("Received %v, %v; wanted true, nil", got, err)
	}
	got, err = mountinfoHasMountpoint(strings.NewReader(info), "/mnt/data root/volumes/b-x")
	if err != nil || got {
		t.Errorf("parent of a mount point: received %v, %v; wanted false, nil", got, err)
	}
	if u := unescapeMountinfo(`a\134b\040c\04`); u != `a\b c\04` {
		t.Errorf("unescapeMountinfo received %q", u)
	}
}

// When the volume's directory cannot be seen, the answer is "cannot tell", which callers
// treat as mounted. A missing directory must not read as "not mounted".
func TestVolumeMountpointMountedFailsSafe(t *testing.T) {
	if _, err := volumeMountpointMounted(""); err == nil {
		t.Error("empty mountpoint: received nil error, wanted one")
	}
	missing := t.TempDir() + "/gone/_data"
	if _, err := volumeMountpointMounted(missing); err == nil {
		t.Error("missing volume directory: received nil error, wanted one")
	}
	dir := t.TempDir()
	if err := os.Mkdir(dir+"/_data", 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := volumeMountpointMounted(dir + "/_data"); err != nil || got {
		t.Errorf("visible, unmounted: received %v, %v; wanted false, nil", got, err)
	}
}
