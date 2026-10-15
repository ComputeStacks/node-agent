package borg

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"cs-agent/containermgr"

	"github.com/spf13/viper"
)

// The vanished-path verification against real borg, in the production image, driven
// through the agent's own code: Archive.Create (and with it the real createCommand, start
// stamp and all), and the real listing commands streamed through
// containermgr.Container.ExecStream into the real Go comparison.
//
// Gated on BORG_IMAGE_TEST=1 because it needs a docker daemon and the image; BORG_IMAGE
// overrides the image. It runs, for example, as
//
//	BORG_IMAGE_TEST=1 go test -run TestVanishedVerificationAgainstBorgImage -v ./backup/borg/
//
// Deterministic, not timing-dependent: borg walks a directory's entries in inode order,
// so a large file created first in the mutated directory is walked first, and the mutation
// fires while borg holds that file open — after borg listed the directory, before it
// stat'ed the rest. A watcher in the container polls /proc/*/fd for the file.
const (
	borgImageTestEnv     = "BORG_IMAGE_TEST"
	defaultBorgTestImage = "ghcr.io/computestacks/cs-docker-borg:1.5"
)

// The fixture every scenario starts from. $BIGDIR names the directory whose entries the
// scenario mutates; big.bin is created in it first so it has the lowest inode there.
// Beyond the scenario's own tree it carries the shapes the comparison has to get right:
// non-UTF-8 names (one with two adjacent invalid bytes), a newline and a space in a name,
// a symlink, a fifo, a hardlink, a valid and an invalid CACHEDIR.TAG, and WordPress's write
// probe. The trailing sleep puts every fixture timestamp strictly before the start stamp.
const verifyFixtureScript = `set -eu
D=/mnt/data
find $D -mindepth 1 -delete
cd $D
mkdir -p "$BIGDIR"
head -c 256M /dev/urandom > "$BIGDIR/big.bin"
F=html/wordpress/wp-content/uploads/fc/site/Files
for a in 00 1a e8; do for b in 00 60 ff; do
  mkdir -p $F/$a/$b
  for i in 1 2 3; do echo "$a$b$i" > $F/$a/$b/$a$b$i.txt; done
done; done
mkdir -p html/wordpress/wp-content/uploads/2026/10
i=1; while [ $i -le 50 ]; do echo "photo $i" > html/wordpress/wp-content/uploads/2026/10/img$i.jpg; i=$((i+1)); done
echo keep > html/wordpress/wp-config.php
mkdir -p odd
printf x > "odd/$(printf 'caf\351.jpg')"
printf x > "odd/$(printf 'a\351\352b')"
printf x > "odd/$(printf 'nl\nname')"
printf x > "odd/sp ace"
ln -s ../html/wordpress/wp-config.php odd/link
mkfifo odd/fifo
echo z > odd/hard1; ln odd/hard1 odd/hard2
mkdir -p cache/sub; printf 'Signature: 8a477f597d28d172789f06886806bc55' > cache/CACHEDIR.TAG; echo y > cache/sub/f
mkdir -p notcache; printf 'Signature: 8a477f597d28d172789f06886806bc5X' > notcache/CACHEDIR.TAG
echo probe > html/wordpress/wp-content/temp-write-test-6a7a669ec0ba20-05577252
sleep 1
`

// The watcher: say it is ready, wait (bounded) for borg to open big.bin, mutate, report.
const verifyWatcherScript = `F=html/wordpress/wp-content/uploads/fc/site/Files
U=html/wordpress/wp-content/uploads/2026
cd /mnt/data
echo ready
end=$(( $(date +%s) + 120 ))
while [ "$(date +%s)" -lt "$end" ]; do
  if ls -l /proc/[0-9]*/fd 2>/dev/null | grep -q big.bin; then
    case "$SCEN" in
      delete) rm -f $F/e8/60/e8601.txt ;;
      rmdir) rm -rf $U/10 ;;
      rename) mv $U/10 $U/10-renamed ;;
      renamemod) mv $U/10 $U/10-renamed; for x in $U/10-renamed/*; do echo changed > "$x"; done ;;
      filerename) mv $F/e8/60/e8602.txt $F/e8/60/e8602-renamed.txt ;;
      oldmtime) rm -f $F/e8/60/e8601.txt; echo unpacked > $F/e8/60/unpacked.txt; touch -d 2020-01-01 $F/e8/60/unpacked.txt ;;
      cache) for x in $F/e8/60/*.txt; do rm -f "$x"; done; mkdir -p $F/7c/11; echo new > $F/7c/11/fresh.txt ;;
    esac
    echo mutated
    exit 0
  fi
done
echo never
exit 1
`

func TestVanishedVerificationAgainstBorgImage(t *testing.T) {
	if os.Getenv(borgImageTestEnv) != "1" {
		t.Skip("set " + borgImageTestEnv + "=1 to run against the borg image (needs docker)")
	}
	image := os.Getenv("BORG_IMAGE")
	if image == "" {
		image = defaultBorgTestImage
	}

	for key, value := range map[string]string{
		"docker.version":                "1.44",
		"backups.borg.compression":      "lz4",
		"backups.borg.lock_wait":        "5",
		"backups.borg.lock_wait_create": "60",
	} {
		previous := viper.Get(key)
		viper.Set(key, value)
		t.Cleanup(func() { viper.Set(key, previous) })
	}

	docker := func(timeout time.Duration, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// Anonymous volume at /mnt/data, as the backup container mounts the customer volume.
	ctr := docker(2*time.Minute, "run", "-d", "-v", "/mnt/data",
		"-e", "BORG_REPO=/tmp/repo", "-e", "BORG_PASSPHRASE=x",
		"-e", "BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes", "-e", "BORG_BASE_DIR=/tmp/borgbase",
		image, "sleep", "3600")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", ctr).Run() })
	docker(time.Minute, "exec", ctr, "borg", "init", "-e", "none", "/tmp/repo")

	container := &containermgr.Container{ID: ctr}
	repo := &Repository{Name: "verify-itest", Container: container}

	const (
		files   = "html/wordpress/wp-content/uploads/fc/site/Files/e8/60"
		uploads = "html/wordpress/wp-content/uploads/2026"
	)
	for _, sc := range []struct {
		name        string
		bigDir      string
		wantMissing int
		wantFirst   string
	}{
		{name: "delete", bigDir: files},
		{name: "rmdir", bigDir: uploads},
		{name: "cache", bigDir: files},
		{name: "rename", bigDir: uploads, wantMissing: 51, wantFirst: uploads + "/10-renamed"},
		{name: "renamemod", bigDir: uploads, wantMissing: 1, wantFirst: uploads + "/10-renamed"},
		{name: "filerename", bigDir: files, wantMissing: 1, wantFirst: files + "/e8602-renamed.txt"},
		{name: "oldmtime", bigDir: files, wantMissing: 1, wantFirst: files + "/unpacked.txt"},
	} {
		t.Run(sc.name, func(t *testing.T) {
			docker(3*time.Minute, "exec", "-e", "BIGDIR="+sc.bigDir, ctr, "sh", "-c", verifyFixtureScript)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			watcher := exec.CommandContext(ctx, "docker", "exec", "-e", "SCEN="+sc.name, ctr, "sh", "-c", verifyWatcherScript)
			stdout, err := watcher.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := watcher.Start(); err != nil {
				t.Fatal(err)
			}
			lines := bufio.NewScanner(stdout)
			if !lines.Scan() || lines.Text() != "ready" {
				t.Fatalf("watcher did not start: %q", lines.Text())
			}

			a := &Archive{Name: sc.name, Repository: repo}
			_, createErr := a.Create()

			mutated := lines.Scan() && lines.Text() == "mutated"
			_ = watcher.Wait()
			if !mutated {
				t.Fatalf("the mutation never fired while borg held big.bin open (%q)", lines.Text())
			}

			if sc.wantMissing == 0 {
				if createErr != nil {
					t.Fatalf("Create failed with (%s) %q, wanted a verified success", createErr.MsgID, createErr.Message)
				}
				if a.Warning == nil || a.Warning.MsgID != msgIDFileNotFound ||
					!strings.Contains(a.Warning.Message, "path(s) vanished during the backup") ||
					!strings.HasSuffix(a.Warning.Message, "every file and directory on the volume older than the backup is in the archive") {
					t.Fatalf("Received warning %+v, wanted the verified vanished-path warning", a.Warning)
				}
				t.Logf("PASS as expected; backup_warning: (%s) %s", a.Warning.MsgID, a.Warning.Message)
			} else {
				want := strings.TrimSpace(strings.Join([]string{
					strconv.Itoa(sc.wantMissing), "path(s) on the volume older than the backup are not in the archive at that path (first:",
					sc.wantFirst + ")"}, " "))
				if createErr == nil {
					t.Fatalf("Create succeeded (warning %+v), wanted it to fail with %q", a.Warning, want)
				}
				if createErr.MsgID != msgIDFileNotFound || createErr.Message != want {
					t.Fatalf("Received (%s) %q, wanted (BackupFileNotFoundError) %q", createErr.MsgID, createErr.Message, want)
				}
				t.Logf("FAIL as expected; reason: (%s) %s", createErr.MsgID, createErr.Message)
			}

			// The same verdict, read directly: the count and the first path.
			vctx, vcancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer vcancel()
			got, err := verifyVanishedWith(vctx, container.ExecStream, a.archivePath())
			if err != nil {
				t.Fatalf("verifyVanishedWith: %v", err)
			}
			if got.missing != sc.wantMissing {
				t.Fatalf("Received %d missing (sample %q), wanted %d", got.missing, got.sample, sc.wantMissing)
			}
			if sc.wantMissing > 0 && got.sample[0] != sc.wantFirst {
				t.Errorf("Received first missing %q, wanted %q", got.sample[0], sc.wantFirst)
			}

			if sc.name == "delete" {
				// The fixture's odd names reached the comparison the way borg renders them:
				// a PASS above already means they matched, and this pins the rendering itself.
				var archive map[string]struct{}
				if err := runListing(vctx, container.ExecStream, "borg list", archiveListingCommand(a.archivePath()), func(r io.Reader) error {
					var perr error
					archive, perr = parseArchiveListing(r)
					return perr
				}); err != nil {
					t.Fatal(err)
				}
				for _, p := range []string{"odd/caf?.jpg", "odd/a??b", "odd/nl\nname", "odd/sp ace", "odd/link", "odd/fifo", "odd/hard2", "notcache/CACHEDIR.TAG"} {
					if _, ok := archive[p]; !ok {
						t.Errorf("archive listing has no %q", p)
					}
				}
				for _, p := range []string{"cache", "cache/sub/f", "html/wordpress/wp-content/temp-write-test-6a7a669ec0ba20-05577252"} {
					if _, ok := archive[p]; ok {
						t.Errorf("archive listing has %q, which borg was told to exclude", p)
					}
				}
			}
		})
	}

	t.Run("nonexistent archive fails closed", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		got, err := verifyVanishedWith(ctx, container.ExecStream, "::does-not-exist")
		if err == nil {
			t.Fatalf("Received %+v and no error, wanted an error", got)
		}
		if !strings.Contains(err.Error(), "Archive.DoesNotExist") {
			t.Errorf("Received %v, wanted borg's Archive.DoesNotExist record", err)
		}
		t.Logf("error as expected: %v", err)
	})
}
