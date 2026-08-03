package borg

import (
	"testing"

	"github.com/spf13/viper"
)

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
