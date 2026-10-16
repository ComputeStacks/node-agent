package borg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	volumeTypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/spf13/viper"
)

// ensureCacheVolume's NFS re-create against a real docker daemon.
//
// Gated on DOCKER_VOLUME_TEST=1 because it needs a docker daemon. No NFS server is needed:
// a local volume of type nfs is only mounted when a container using it STARTS, and none of
// these do. DOCKER_VOLUME_TEST_IMAGE overrides the image used for the stopped container and
// for writing into a plain volume (default alpine:latest).
//
// The re-create cases must run as root: before removing a volume the agent proves the
// volume's directory under docker's data root is not mounted, and an unprivileged user
// cannot see that directory, which the agent rightly treats as "cannot tell — leave it".
// Those cases skip when not root. Build the test binary and run it with sudo:
//
//	go test -c -o /tmp/borg.test ./backup/borg/
//	sudo DOCKER_VOLUME_TEST=1 /tmp/borg.test -test.run 'TestDockerVolume' -test.v
const (
	dockerVolumeTestEnv      = "DOCKER_VOLUME_TEST"
	dockerVolumeTestImageEnv = "DOCKER_VOLUME_TEST_IMAGE"
	defaultVolumeTestImage   = "alpine:latest"
	legacyNFSMountOpts       = "addr=10.1.2.3,rw,nfsvers=4,async,noatime,rsize=32768,wsize=32768"
)

// dockerVolumeTestClient skips unless the docker-backed tests were asked for, sets up the
// NFS backend configuration, and returns a client plus a fresh repository named so that it
// cannot collide with anything real. The repository's volume and every container labeled
// for it are removed when the test ends.
func dockerVolumeTestClient(t *testing.T) (*client.Client, *Repository) {
	t.Helper()
	if os.Getenv(dockerVolumeTestEnv) != "1" {
		t.Skip("set " + dockerVolumeTestEnv + "=1 to run the docker-backed volume tests")
	}
	nfsBackendConfig()
	viper.Set("docker.version", "1.44")
	// The absent path would otherwise SSH to the NFS server to create the directory.
	viper.Set("backups.borg.nfs_create_path", false)

	cli, err := client.NewClientWithOpts(client.WithVersion("1.44"))
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}

	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	r := &Repository{Name: "cs-agent-voltest-" + hex.EncodeToString(buf)}

	t.Cleanup(func() {
		ctx := context.Background()
		containers, _ := cli.ContainerList(ctx, container.ListOptions{All: true})
		for _, c := range containers {
			if c.Labels["cs-agent-voltest"] == r.Name {
				_ = cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
			}
		}
		_ = cli.VolumeRemove(ctx, "b-"+r.Name, true)
		_ = cli.Close()
	})
	return cli, r
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to see docker's volume directory; run the compiled test binary with sudo")
	}
}

func volumeTestImage() string {
	if img := os.Getenv(dockerVolumeTestImageEnv); img != "" {
		return img
	}
	return defaultVolumeTestImage
}

// createTestVolume declares b-<r.Name> the way the agent's normal create path would, with
// the given driver options.
func createTestVolume(t *testing.T, cli *client.Client, r *Repository, driverOpts map[string]string) volumeTypes.Volume {
	t.Helper()
	v, err := cli.VolumeCreate(context.Background(), volumeTypes.CreateOptions{
		Name:       "b-" + r.Name,
		Driver:     "local",
		DriverOpts: driverOpts,
		Labels: map[string]string{
			"com.computestacks.role": "backup",
			"com.computestacks.for":  r.Name,
		},
	})
	if err != nil {
		t.Fatalf("creating test volume: %v", err)
	}
	return v
}

func inspectTestVolume(t *testing.T, cli *client.Client, r *Repository) volumeTypes.Volume {
	t.Helper()
	v, err := cli.VolumeInspect(context.Background(), "b-"+r.Name)
	if err != nil {
		t.Fatalf("inspecting test volume: %v", err)
	}
	return v
}

// legacyNFSOpts is a volume as the agent declared it before the version became
// configurable: same server and directory as the test configuration, nfsvers=4.
func legacyNFSOpts(r *Repository) map[string]string {
	desired := nfsVolumeDriverOpts(r.Name)
	return map[string]string{"type": "nfs", "device": desired["device"], "o": legacyNFSMountOpts}
}

// Plan test 6: a volume that differs only in its mount options is re-created with the
// current ones, and keeps its labels.
func TestDockerVolumeRecreatesStaleNFSVolume(t *testing.T) {
	cli, r := dockerVolumeTestClient(t)
	requireRoot(t)
	createTestVolume(t, cli, r, legacyNFSOpts(r))

	if ok, err := r.ensureCacheVolume(cli); err != nil || !ok {
		t.Fatalf("ensureCacheVolume = %v, %v; wanted true, nil", ok, err)
	}

	desired := nfsVolumeDriverOpts(r.Name)
	got := inspectTestVolume(t, cli, r)
	if err := verifyNFSVolume(got, desired); err != nil {
		t.Errorf("volume was not re-created with the current options: %v", err)
	}
	if !strings.Contains(got.Options["o"], "nfsvers=4.2") {
		t.Errorf("Received o %q, wanted nfsvers=4.2", got.Options["o"])
	}
	if got.Labels["com.computestacks.role"] != "backup" || got.Labels["com.computestacks.for"] != r.Name {
		t.Errorf("Received labels %v, wanted the backup role and for=%s", got.Labels, r.Name)
	}

	// A second call finds it current and leaves it be.
	if ok, err := r.ensureCacheVolume(cli); err != nil || !ok {
		t.Fatalf("second ensureCacheVolume = %v, %v; wanted true, nil", ok, err)
	}
	if again := inspectTestVolume(t, cli, r); again.CreatedAt != got.CreatedAt {
		t.Errorf("an up-to-date volume was re-created (created %s, then %s)", got.CreatedAt, again.CreatedAt)
	}
}

// Plan test 7: a stopped container that references the volume makes docker refuse the
// remove; the operation proceeds on the existing volume, which keeps its old options.
func TestDockerVolumeBusyVolumeIsLeftAlone(t *testing.T) {
	cli, r := dockerVolumeTestClient(t)
	requireRoot(t)
	before := createTestVolume(t, cli, r, legacyNFSOpts(r))

	ctx := context.Background()
	if _, err := cli.ContainerCreate(ctx, &container.Config{
		Image:  volumeTestImage(),
		Cmd:    []string{"true"},
		Labels: map[string]string{"cs-agent-voltest": r.Name},
	}, &container.HostConfig{
		Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: "b-" + r.Name, Target: "/mnt/borg"}},
	}, nil, nil, "cs-agent-voltest-"+r.Name); err != nil {
		t.Fatalf("creating the referencing container: %v", err)
	}

	if ok, err := r.ensureCacheVolume(cli); err != nil || !ok {
		t.Fatalf("ensureCacheVolume = %v, %v; wanted true, nil", ok, err)
	}

	got := inspectTestVolume(t, cli, r)
	if got.Options["o"] != legacyNFSMountOpts {
		t.Errorf("Received o %q on a volume in use, wanted it unchanged: %q", got.Options["o"], legacyNFSMountOpts)
	}
	if got.CreatedAt != before.CreatedAt {
		t.Errorf("a volume in use was re-created (created %s, then %s)", before.CreatedAt, got.CreatedAt)
	}
}

// Plan test 8: a volume pointing at a different directory is not re-pointed — that would
// leave borg looking at an empty location.
func TestDockerVolumeDifferentDeviceIsLeftAlone(t *testing.T) {
	cli, r := dockerVolumeTestClient(t)
	opts := legacyNFSOpts(r)
	opts["device"] = ":/somewhere/else/b-" + r.Name
	before := createTestVolume(t, cli, r, opts)

	if ok, err := r.ensureCacheVolume(cli); err != nil || !ok {
		t.Fatalf("ensureCacheVolume = %v, %v; wanted true, nil", ok, err)
	}

	got := inspectTestVolume(t, cli, r)
	if got.Options["device"] != opts["device"] || got.Options["o"] != opts["o"] || got.CreatedAt != before.CreatedAt {
		t.Errorf("Received %v (created %s), wanted the volume untouched: %v (created %s)", got.Options, got.CreatedAt, opts, before.CreatedAt)
	}
}

// Plan test 9: off the NFS backend a b-* volume IS the repository and is never touched.
func TestDockerVolumeLocalBackendIsNeverTouched(t *testing.T) {
	cli, r := dockerVolumeTestClient(t)
	viper.Set("backups.borg.nfs", false)
	before := createTestVolume(t, cli, r, nil)

	volName := "b-" + r.Name
	run := func(script string) (string, error) {
		out, err := exec.Command("docker", "run", "--rm", "--label", "cs-agent-voltest="+r.Name,
			"-v", volName+":/mnt/borg", volumeTestImage(), "sh", "-c", script).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run("echo keep > /mnt/borg/marker"); err != nil {
		t.Fatalf("writing marker: %v: %s", err, out)
	}

	if ok, err := r.ensureCacheVolume(cli); err != nil || !ok {
		t.Fatalf("ensureCacheVolume = %v, %v; wanted true, nil", ok, err)
	}

	got := inspectTestVolume(t, cli, r)
	if got.CreatedAt != before.CreatedAt || len(got.Options) != 0 {
		t.Errorf("local backend volume changed: %v (created %s, was %s)", got.Options, got.CreatedAt, before.CreatedAt)
	}
	if out, err := run("cat /mnt/borg/marker"); err != nil || out != "keep" {
		t.Errorf("marker after ensureCacheVolume: %q, %v; wanted \"keep\"", out, err)
	}
}

// The absent path on the NFS backend declares the volume with the current options.
func TestDockerVolumeAbsentNFSVolumeIsCreatedCurrent(t *testing.T) {
	cli, r := dockerVolumeTestClient(t)

	if ok, err := r.ensureCacheVolume(cli); err != nil || !ok {
		t.Fatalf("ensureCacheVolume = %v, %v; wanted true, nil", ok, err)
	}
	if err := verifyNFSVolume(inspectTestVolume(t, cli, r), nfsVolumeDriverOpts(r.Name)); err != nil {
		t.Errorf("new volume: %v", err)
	}
}

// Post-create verification: docker's VolumeCreate on a name that already exists returns the
// existing volume and ignores the options. If the name was taken by a plain local volume —
// what docker auto-creates for a container mounting a missing volume — the re-create must
// fail rather than let borg run against it.
func TestDockerVolumeCreateIsVerified(t *testing.T) {
	cli, r := dockerVolumeTestClient(t)
	createTestVolume(t, cli, r, nil)

	err := r.createAndVerifyNFSVolume(context.Background(), cli, nfsVolumeDriverOpts(r.Name))
	if err == nil {
		t.Fatal("Received nil, wanted an error: the volume docker returned is not an NFS mount")
	}
	if got := inspectTestVolume(t, cli, r); len(got.Options) != 0 {
		t.Errorf("the pre-existing plain volume changed: %v", got.Options)
	}
}

// R5: when the agent cannot see docker's volume directory it cannot prove the volume is not
// mounted, and a stale volume is then left alone rather than removed. Only meaningful when
// NOT root, which is exactly when the re-create cases above skip.
func TestDockerVolumeUnverifiableMountpointIsLeftAlone(t *testing.T) {
	cli, r := dockerVolumeTestClient(t)
	if os.Geteuid() == 0 {
		t.Skip("root can see docker's volume directory; this case needs an unprivileged user")
	}
	before := createTestVolume(t, cli, r, legacyNFSOpts(r))
	if _, err := volumeMountpointMounted(before.Mountpoint); err == nil {
		t.Skip("this user can see docker's volume directory")
	}

	if ok, err := r.ensureCacheVolume(cli); err != nil || !ok {
		t.Fatalf("ensureCacheVolume = %v, %v; wanted true, nil", ok, err)
	}
	if got := inspectTestVolume(t, cli, r); got.Options["o"] != legacyNFSMountOpts || got.CreatedAt != before.CreatedAt {
		t.Errorf("Received %v (created %s), wanted the volume untouched", got.Options, got.CreatedAt)
	}
}
