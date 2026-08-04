package borg

import (
	"cs-agent/types"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
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
	labels, env, mounts := containerSpec(target, "vol-owner")

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

	labels, env, mounts := containerSpec(&types.Volume{Name: "vol-1"}, "vol-1")

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

	labels, env, _ := containerSpec(&types.Volume{Name: "vol-1"}, "vol-1")

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

			_, _, mounts := containerSpec(&types.Volume{Name: "vol-1", Trash: true}, "vol-1")

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

	_, env, _ := containerSpec(&types.Volume{Name: "vol-1"}, "vol-1")

	if got, ok := borgEnvValue(env, "BORG_REMOTE_PATH"); !ok || got != "/usr/local/bin/borg" {
		t.Errorf("Received BORG_REMOTE_PATH %q (present=%v), wanted %q", got, ok, "/usr/local/bin/borg")
	}
	if got, ok := borgEnvValue(env, "BORG_RSH"); !ok || got != "ssh -i /etc/computestacks/borg.key" {
		t.Errorf("Received BORG_RSH %q (present=%v), wanted %q", got, ok, "ssh -i /etc/computestacks/borg.key")
	}

	viper.Reset()
	_, localEnv, _ := containerSpec(&types.Volume{Name: "vol-1"}, "vol-1")

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
