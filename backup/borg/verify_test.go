package borg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

func TestVerifiableVanishedCreate(t *testing.T) {
	var (
		vanished          = backupFileNotFoundErrorRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
		vanishedAndTorn   = fileChangedWarningRecord + "\r\n" + backupFileNotFoundErrorRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
		tornOnly          = fileChangedWarningRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
		vanishedAndDenied = backupFileNotFoundErrorRecord + "\r\n" + backupPermissionErrorRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
		vanishedAndRaced  = backupFileNotFoundErrorRecord + "\r\n" + backupRaceConditionErrorRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
	)
	for _, i := range []struct {
		name      string
		res       ExecResult
		decoded   bool
		archiveID string
		want      bool
	}{
		{name: "vanished only", res: ExecResult{ExitCode: borgWarningExit, Response: vanished}, decoded: true, archiveID: "a", want: true},
		{name: "vanished and a torn read", res: ExecResult{ExitCode: borgWarningExit, Response: vanishedAndTorn}, decoded: true, archiveID: "a", want: true},
		// wroteCompleteArchive already accepts this one; nothing to verify.
		{name: "torn read only", res: ExecResult{ExitCode: borgWarningExit, Response: tornOnly}, decoded: true, archiveID: "a", want: false},
		// A definite loss beside the vanished path: the verification must not excuse it.
		{name: "vanished and permission denied", res: ExecResult{ExitCode: borgWarningExit, Response: vanishedAndDenied}, decoded: true, archiveID: "a", want: false},
		{name: "vanished and raced", res: ExecResult{ExitCode: borgWarningExit, Response: vanishedAndRaced}, decoded: true, archiveID: "a", want: false},
		{name: "docker fault", res: ExecResult{ExitCode: borgWarningExit, DockerFault: true, Response: vanished}, decoded: true, archiveID: "a", want: false},
		{name: "error tier", res: ExecResult{ExitCode: 2, Response: vanished}, decoded: true, archiveID: "a", want: false},
		{name: "payload not decoded", res: ExecResult{ExitCode: borgWarningExit, Response: vanished}, decoded: false, archiveID: "a", want: false},
		{name: "empty archive id", res: ExecResult{ExitCode: borgWarningExit, Response: vanished}, decoded: true, archiveID: "", want: false},
		{name: "no records", res: ExecResult{ExitCode: borgWarningExit, Response: createJSONPayloadTTY}, decoded: true, archiveID: "a", want: false},
	} {
		t.Run(i.name, func(t *testing.T) {
			if got := verifiableVanishedCreate(i.res, i.decoded, i.archiveID); got != i.want {
				t.Errorf("Received %v, wanted %v", got, i.want)
			}
		})
	}
}

// Times for the comparison table: the start stamp, a tick before it, and after it.
var (
	vStart = fsTimestamp{sec: 1000, nsec: 500}
	vOld   = fsTimestamp{sec: 900}
	vNew   = fsTimestamp{sec: 1001}
)

// ent builds a volume entry the way find reports it, with its leading "./".
func ent(typ byte, mtime, ctime fsTimestamp, path string) volumeEntry {
	return volumeEntry{typ: typ, mtime: mtime, ctime: ctime, path: "./" + path}
}

func set(paths ...string) map[string]struct{} {
	m := map[string]struct{}{".": {}}
	for _, p := range paths {
		m[p] = struct{}{}
	}
	return m
}

// renamedDir is the measured rename scenario as data: a directory of 50 files renamed while
// borg walked its parent. borg lost it under both names, so the archive has neither; on the
// volume the moved directory keeps its mtime (its ctime moves) and so do its files.
func renamedDir(rewriteFiles bool) []volumeEntry {
	vol := []volumeEntry{
		{typ: 'd', mtime: vOld, ctime: vOld, path: "."},
		ent('d', vNew, vNew, "u"), // its entry list changed
		ent('f', vOld, vOld, "u/big.bin"),
		ent('d', vOld, vNew, "u/10-renamed"),
	}
	for n := 1; n <= 50; n++ {
		m := vOld
		if rewriteFiles {
			m = vNew
		}
		vol = append(vol, ent('f', m, vNew, "u/10-renamed/img"+strconv.Itoa(n)+".jpg"))
	}
	return vol
}

func TestCompareVolumeToArchive(t *testing.T) {
	for _, i := range []struct {
		name        string
		vol         []volumeEntry
		archive     map[string]struct{}
		cacheDirs   []string
		wantMissing int
		wantSample  []string
	}{
		{
			// The deleted file is on neither side.
			name:    "delete",
			vol:     []volumeEntry{ent('d', vNew, vNew, "d"), ent('f', vOld, vOld, "d/a"), ent('f', vOld, vOld, "d/c")},
			archive: set("d", "d/a", "d/c"),
		},
		{
			name:    "rmdir",
			vol:     []volumeEntry{ent('d', vNew, vNew, "u"), ent('f', vOld, vOld, "u/big.bin")},
			archive: set("u", "u/big.bin"),
		},
		{
			// Files removed and new ones created under new names in a new directory.
			name:    "cache churn",
			vol:     []volumeEntry{ent('d', vNew, vNew, "c"), ent('d', vNew, vNew, "c/7c"), ent('f', vNew, vNew, "c/7c/fresh.txt")},
			archive: set("c"),
		},
		{
			name:        "rename",
			vol:         renamedDir(false),
			archive:     set("u", "u/big.bin"),
			wantMissing: 51,
			wantSample:  []string{"u/10-renamed", "u/10-renamed/img1.jpg", "u/10-renamed/img10.jpg", "u/10-renamed/img11.jpg", "u/10-renamed/img12.jpg"},
		},
		{
			// Every file rewritten after the rename carries no old timestamp; the directory
			// entry, whose mtime survives the rename, is what still catches it.
			name:        "rename then modify every file",
			vol:         renamedDir(true),
			archive:     set("u", "u/big.bin"),
			wantMissing: 1,
			wantSample:  []string{"u/10-renamed"},
		},
		{
			// A directory moved to a different parent mid-walk: measured to keep its mtime.
			name: "cross-parent directory move",
			vol: []volumeEntry{
				ent('d', vNew, vNew, "a"), ent('d', vNew, vNew, "b"),
				ent('d', vOld, vNew, "b/x"), ent('f', vOld, vNew, "b/x/f"),
			},
			archive:     set("a", "b"),
			wantMissing: 2,
			wantSample:  []string{"b/x", "b/x/f"},
		},
		{
			name:        "file rename",
			vol:         []volumeEntry{ent('d', vNew, vNew, "d"), ent('f', vOld, vNew, "d/e8602-renamed.txt")},
			archive:     set("d"),
			wantMissing: 1,
			wantSample:  []string{"d/e8602-renamed.txt"},
		},
		{
			// A false FAIL, by design: a file written mid-walk with an old mtime.
			name:        "new file with an old mtime",
			vol:         []volumeEntry{ent('d', vNew, vNew, "d"), ent('f', fsTimestamp{sec: 1}, vNew, "d/unpacked.txt")},
			archive:     set("d"),
			wantMissing: 1,
			wantSample:  []string{"d/unpacked.txt"},
		},
		{
			// A false FAIL, by design: borg archived a/f, then it moved to b/f.
			name:        "move after borg walked it",
			vol:         []volumeEntry{ent('d', vNew, vNew, "b"), ent('f', vOld, vNew, "b/f")},
			archive:     set("a", "a/f", "b"),
			wantMissing: 1,
			wantSample:  []string{"b/f"},
		},
		{
			// borg's --exclude-caches drops the tagged directory itself and everything under
			// it (measured); a sibling that merely shares the prefix is still checked.
			name: "cache-tagged directory excluded",
			vol: []volumeEntry{
				ent('d', vOld, vOld, "c"), ent('f', vOld, vOld, "c/CACHEDIR.TAG"), ent('d', vOld, vOld, "c/sub"),
				ent('f', vOld, vOld, "c/sub/f"), ent('d', vOld, vOld, "cx"), ent('f', vOld, vOld, "cx/f"),
			},
			archive:     set(),
			cacheDirs:   []string{"./c"},
			wantMissing: 2,
			wantSample:  []string{"cx", "cx/f"},
		},
		{
			// The tag listing reports only a valid signature, so an invalid tag's directory is
			// not in cacheDirs and is compared like any other.
			name:        "invalid cache tag not excluded",
			vol:         []volumeEntry{ent('d', vOld, vOld, "bad"), ent('f', vOld, vOld, "bad/CACHEDIR.TAG")},
			archive:     set(),
			wantMissing: 2,
			wantSample:  []string{"bad", "bad/CACHEDIR.TAG"},
		},
		{
			name:      "volume root tagged",
			vol:       []volumeEntry{ent('f', vOld, vOld, "a")},
			archive:   set(),
			cacheDirs: []string{"."},
		},
		{
			// borg never walks the probe, nor anything under a directory the pattern
			// matches; a customer file that only shares the prefix is still checked.
			name: "write probe skipped",
			vol: []volumeEntry{
				ent('f', vOld, vOld, "html/wp-content/temp-write-test-6a7a669ec0ba20-05577252"),
				ent('d', vOld, vOld, "temp-write-test-ab-12"), ent('f', vOld, vOld, "temp-write-test-ab-12/inner"),
				ent('d', vOld, vOld, "temp-write-test-data"), ent('f', vOld, vOld, "temp-write-test-data/keep.txt"),
			},
			archive:     set("temp-write-test-data"),
			wantMissing: 1,
			wantSample:  []string{"temp-write-test-data/keep.txt"},
		},
		{
			name:        "symlink counted, fifo and socket ignored",
			vol:         []volumeEntry{ent('l', vOld, vOld, "link"), ent('p', vOld, vOld, "fifo"), ent('s', vOld, vOld, "sock")},
			archive:     set(),
			wantMissing: 1,
			wantSample:  []string{"link"},
		},
		{
			name:    "newline and space in names",
			vol:     []volumeEntry{ent('f', vOld, vOld, "nl\nname"), ent('f', vOld, vOld, "sp ace")},
			archive: set("nl\nname", "sp ace"),
		},
		{
			// Measured on borg 1.4.4: each undecodable byte lists as one "?".
			name:    "non-UTF-8 per-byte normalization",
			vol:     []volumeEntry{ent('f', vOld, vOld, "caf\xe9.jpg"), ent('f', vOld, vOld, "a\xe9\xeab"), ent('f', vOld, vOld, "tr\xe2\x82x")},
			archive: set("caf?.jpg", "a??b", "tr??x"),
		},
		{
			// Two adjacent invalid bytes are two "?", never collapsed to one.
			name:        "adjacent invalid bytes are not collapsed",
			vol:         []volumeEntry{ent('f', vOld, vOld, "a\xe9\xeab")},
			archive:     set("a?b"),
			wantMissing: 1,
			wantSample:  []string{"a??b"},
		},
		{
			// Strictly older: a timestamp equal to the stamp is not before the backup.
			name:    "timestamp equal to the start is not older",
			vol:     []volumeEntry{ent('f', vStart, vStart, "same-tick")},
			archive: set(),
		},
		{
			name:        "one nanosecond before the start is older",
			vol:         []volumeEntry{ent('f', vNew, fsTimestamp{sec: 1000, nsec: 499}, "ctime-older")},
			archive:     set(),
			wantMissing: 1,
			wantSample:  []string{"ctime-older"},
		},
		{
			name:        "missing paths are reported sorted and capped",
			vol:         []volumeEntry{ent('f', vOld, vOld, "g"), ent('f', vOld, vOld, "f"), ent('f', vOld, vOld, "e"), ent('f', vOld, vOld, "d"), ent('f', vOld, vOld, "c"), ent('f', vOld, vOld, "b"), ent('f', vOld, vOld, "a\nz")},
			archive:     set(),
			wantMissing: 7,
			wantSample:  []string{`a\nz`, "b", "c", "d", "e"},
		},
	} {
		t.Run(i.name, func(t *testing.T) {
			missing, sample := compareVolumeToArchive(i.vol, vStart, i.archive, i.cacheDirs)
			if missing != i.wantMissing {
				t.Errorf("Received %d missing, wanted %d (sample %q)", missing, i.wantMissing, sample)
			}
			if strings.Join(sample, "|") != strings.Join(i.wantSample, "|") {
				t.Errorf("Received sample %q, wanted %q", sample, i.wantSample)
			}
		})
	}
}

func TestParseFindTimestamp(t *testing.T) {
	for _, i := range []struct {
		in      string
		want    fsTimestamp
		wantErr bool
	}{
		{in: "1792098111.2614808580", want: fsTimestamp{sec: 1792098111, nsec: 261480858}},
		{in: "0.0000000000", want: fsTimestamp{}},
		{in: "5.25", want: fsTimestamp{sec: 5, nsec: 250000000}},
		// Measured: 1.5 s before the epoch prints as tv_sec -2, tv_nsec .5.
		{in: "-2.5000000000", want: fsTimestamp{sec: -2, nsec: 500000000}},
		{in: "-1.9999999990", want: fsTimestamp{sec: -1, nsec: 999999999}},
		{in: "1.0000000001", wantErr: true},
		{in: "1", wantErr: true},
		{in: ".5", wantErr: true},
		{in: "-.5", wantErr: true},
		{in: "+1.5", wantErr: true},
		{in: "1.5x", wantErr: true},
		{in: "1.-5", wantErr: true},
		{in: "", wantErr: true},
	} {
		got, err := parseFindTimestamp(i.in)
		if (err != nil) != i.wantErr {
			t.Errorf("%q: received error %v, wanted error %v", i.in, err, i.wantErr)
			continue
		}
		if !i.wantErr && got != i.want {
			t.Errorf("%q: received %+v, wanted %+v", i.in, got, i.want)
		}
	}
	// The ordering survives the sign convention.
	a, _ := parseFindTimestamp("-2.5000000000")
	b, _ := parseFindTimestamp("-1.9999999990")
	if !a.before(b) || b.before(a) {
		t.Errorf("Received %+v before %+v = %v, wanted true", a, b, a.before(b))
	}
}

func TestParseVolumeListing(t *testing.T) {
	good := "d\x001.5000000000\x002.0000000000\x00.\x00" +
		"f\x001.0000000000\x001.0000000000\x00./nl\nname\x00" +
		"f\x001.0000000000\x001.0000000000\x00./caf\xe9.jpg\x00"
	var got []volumeEntry
	if err := parseVolumeListing(strings.NewReader(good), func(e volumeEntry) { got = append(got, e) }); err != nil {
		t.Fatalf("Received %v, wanted nil", err)
	}
	if len(got) != 3 || got[0].typ != 'd' || got[0].mtime != (fsTimestamp{sec: 1, nsec: 500000000}) ||
		got[1].path != "./nl\nname" || got[2].path != "./caf\xe9.jpg" {
		t.Errorf("Received %+v", got)
	}

	for name, in := range map[string]string{
		"truncated mid-record":    "f\x001.0\x001.0\x00./a\x00f\x001.0\x00",
		"last field unterminated": "f\x001.0\x001.0\x00./a",
		"bad type":                "ff\x001.0\x001.0\x00./a\x00",
		"bad mtime":               "f\x00x\x001.0\x00./a\x00",
		"bad ctime":               "f\x001.0\x00\x00./a\x00",
	} {
		if err := parseVolumeListing(strings.NewReader(in), func(volumeEntry) {}); err == nil {
			t.Errorf("%s: received nil, wanted an error", name)
		}
	}
	if err := parseVolumeListing(strings.NewReader(""), func(volumeEntry) {}); err != nil {
		t.Errorf("empty listing: received %v, wanted nil", err)
	}
}

// Lines captured from `borg list --json-lines` on borg 1.4.4 (the production image).
const (
	archiveListingRoot    = `{"type": "d", "mode": "drwxr-xr-x", "user": 0, "group": 0, "uid": 0, "gid": 0, "path": ".", "healthy": true, "source": "", "linktarget": "", "flags": 0, "mtime": "2026-10-15T21:01:51.261481", "size": 0}`
	archiveListingNonUTF8 = `{"type": "-", "mode": "-rw-r--r--", "user": 0, "group": 0, "uid": 0, "gid": 0, "path": "a??b", "healthy": true, "source": "", "linktarget": "", "flags": 0, "mtime": "2026-10-15T21:01:51.257481", "size": 1}`
	archiveListingNewline = `{"type": "-", "mode": "-rw-r--r--", "user": 0, "group": 0, "uid": 0, "gid": 0, "path": "nl\nname", "healthy": true, "source": "", "linktarget": "", "flags": 0, "mtime": "2026-10-15T21:01:51.257481", "size": 1}`
	archiveListingHardlnk = `{"type": "-", "mode": "hrw-r--r--", "user": 0, "group": 0, "uid": 0, "gid": 0, "path": "f2", "healthy": true, "source": "hl", "linktarget": "hl", "flags": 0, "mtime": "2026-10-15T21:01:51.261481", "size": 0}`
)

func TestParseArchiveListing(t *testing.T) {
	in := archiveListingRoot + "\n" + archiveListingNonUTF8 + "\n\n" + archiveListingNewline + "\n" + archiveListingHardlnk + "\n"
	got, err := parseArchiveListing(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Received %v, wanted nil", err)
	}
	for _, p := range []string{".", "a??b", "nl\nname", "f2"} {
		if _, ok := got[p]; !ok {
			t.Errorf("Received %v, wanted it to contain %q", got, p)
		}
	}
	if len(got) != 4 {
		t.Errorf("Received %d paths, wanted 4", len(got))
	}

	for name, in := range map[string]string{
		"malformed line":        archiveListingRoot + "\n" + `{"path": "x"` + "\n",
		"not an object":         `"just a string"` + "\n",
		"no path":               `{"type": "-"}` + "\n",
		"null":                  "null\n",
		"line beyond the cap":   `{"path": "` + strings.Repeat("a", maxArchiveLineBytes) + `"}` + "\n",
		"non-JSON text mid-way": archiveListingRoot + "\nTraceback (most recent call last):\n",
	} {
		if _, err := parseArchiveListing(strings.NewReader(in)); err == nil {
			t.Errorf("%s: received nil, wanted an error", name)
		}
	}
}

func TestParseNULListAndStamp(t *testing.T) {
	if got, err := parseNULList(strings.NewReader("./c\x00./a b\x00")); err != nil || len(got) != 2 || got[1] != "./a b" {
		t.Errorf("Received %q, %v", got, err)
	}
	if _, err := parseNULList(strings.NewReader("./c\x00./a")); err == nil {
		t.Error("truncated: received nil, wanted an error")
	}
	if got, err := parseNULList(strings.NewReader("")); err != nil || len(got) != 0 {
		t.Errorf("empty: received %q, %v", got, err)
	}

	if got, err := parseStamp(strings.NewReader("1792098111.2614808580\x00")); err != nil || got != (fsTimestamp{sec: 1792098111, nsec: 261480858}) {
		t.Errorf("Received %+v, %v", got, err)
	}
	for _, in := range []string{"", "1.0\x002.0\x00", "1.0", "x\x00"} {
		if _, err := parseStamp(strings.NewReader(in)); err == nil {
			t.Errorf("%q: received nil, wanted an error", in)
		}
	}
}

func TestNormalizeBorgPath(t *testing.T) {
	for in, want := range map[string]string{
		"plain/path":   "plain/path",
		"caf\xe9.jpg":  "caf?.jpg",
		"a\xe9\xeab":   "a??b",
		"tr\xe2\x82x":  "tr??x",
		"\xed\xa0\x80": "???", // an encoded surrogate is not valid UTF-8
		"ok-é-\xff":    "ok-é-?",
	} {
		if got := normalizeBorgPath(in); got != want {
			t.Errorf("%q: received %q, wanted %q", in, got, want)
		}
	}
	if got := safePath("a\nb\tc\x1b\xe9 d"); got != `a\nb\tc\x1b? d` {
		t.Errorf("Received %q", got)
	}
}

// The listing commands run in the borg image exactly as written here; the integration test
// runs them for real. Pinned so a change to any of them is deliberate.
func TestVerifyListingCommands(t *testing.T) {
	if stampListingCmd != `find /tmp/cs-create-start -maxdepth 0 -printf '%T@\0'` {
		t.Errorf("Received %q", stampListingCmd)
	}
	if volumeListingCmd != `cd /mnt/data && find . -xdev -printf '%y\0%T@\0%C@\0%p\0'` {
		t.Errorf("Received %q", volumeListingCmd)
	}
	wantTags := `cd /mnt/data && find . -xdev -type f -name CACHEDIR.TAG -exec sh -c '[ "$(head -c 43 "$1")" = "Signature: 8a477f597d28d172789f06886806bc55" ]' sh {} \; -printf '%h\0'`
	if cacheTagListingCmd != wantTags {
		t.Errorf("Received %q, wanted %q", cacheTagListingCmd, wantTags)
	}
	if len(cacheDirTagSignature) != 43 {
		t.Errorf("Received a %d-byte signature, wanted 43: head -c 43 reads exactly that many", len(cacheDirTagSignature))
	}
	if got := archiveListingCommand("::auto-1"); got != "borg --log-json list --bypass-lock --json-lines ::auto-1" {
		t.Errorf("Received %q", got)
	}
	// One source of truth for the write-probe pattern.
	if wpWriteProbeExclude != `'re:`+wpWriteProbeRE.String()+`'` {
		t.Errorf("Received exclude %q and verification pattern %q, wanted the same pattern", wpWriteProbeExclude, wpWriteProbeRE.String())
	}
}

func TestVanishedWarning(t *testing.T) {
	res := ExecResult{ExitCode: borgWarningExit, Response: backupFileNotFoundErrorRecord + "\r\n" + backupFileNotFoundErrorRecord + "\r\n" + createJSONPayloadTTY}
	got := vanishedWarning(res)
	want := "2 path(s) vanished during the backup (first: f08169); every file and directory on the volume older than the backup is in the archive"
	if got.MsgID != "BackupFileNotFoundError" || got.Message != want {
		t.Errorf("Received (%s) %q, wanted (BackupFileNotFoundError) %q", got.MsgID, got.Message, want)
	}

	res.Response = fileChangedWarningRecord + "\r\n" + backupFileNotFoundErrorRecord + "\r\n" + fileChangedWarningRecord + "\r\n" + createJSONPayloadTTY
	got = vanishedWarning(res)
	want = "1 path(s) vanished during the backup (first: f08169); every file and directory on the volume older than the backup is in the archive; 2 file(s) changed while being read"
	if got.Message != want {
		t.Errorf("Received %q, wanted %q", got.Message, want)
	}
}

func TestVerificationFailure(t *testing.T) {
	a := &Archive{Name: "auto-1", Repository: &Repository{Name: "vol"}}
	res := ExecResult{ExitCode: borgWarningExit, Response: backupFileNotFoundErrorRecord + "\r\n" + createJSONPayloadTTY}
	got := a.verificationFailure(res, verifyResult{missing: 51, sample: []string{"u/10-renamed", "u/10-renamed/img1.jpg"}})
	want := "51 path(s) on the volume older than the backup are not in the archive at that path (first: u/10-renamed)"
	if got.MsgID != "BackupFileNotFoundError" || got.Message != want {
		t.Errorf("Received (%s) %q, wanted (BackupFileNotFoundError) %q", got.MsgID, got.Message, want)
	}
}

// fakeListings answers verifyVanishedWith's execs by command.
type fakeListing struct {
	stdout string
	code   int
	stderr string
	err    error
}

func fakeExec(listings map[string]fakeListing) streamExec {
	return func(ctx context.Context, cmd []string, w io.Writer) (int, string, error) {
		if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
			return 1, "", errors.New("unexpected argv")
		}
		l, ok := listings[cmd[2]]
		if !ok {
			return 127, "unknown command " + cmd[2], nil
		}
		if _, err := io.WriteString(w, l.stdout); err != nil {
			return 1, "", err
		}
		return l.code, l.stderr, l.err
	}
}

func TestVerifyVanishedWith(t *testing.T) {
	const archivePath = "::auto-1"
	base := func() map[string]fakeListing {
		return map[string]fakeListing{
			stampListingCmd:                    {stdout: "1000.0000000000\x00"},
			archiveListingCommand(archivePath): {stdout: archiveListingRoot + "\n" + `{"type": "-", "path": "kept"}` + "\n"},
			cacheTagListingCmd:                 {stdout: "./c\x00"},
			volumeListingCmd: {stdout: "d\x00900.0\x00900.0\x00.\x00" +
				"f\x00900.0\x00900.0\x00./kept\x00" +
				"f\x00900.0\x00900.0\x00./c/cached\x00" +
				"f\x001001.0\x001001.0\x00./new\x00"},
		}
	}

	t.Run("pass", func(t *testing.T) {
		got, err := verifyVanishedWith(context.Background(), fakeExec(base()), archivePath)
		if err != nil || got.missing != 0 {
			t.Errorf("Received %+v, %v; wanted 0 missing", got, err)
		}
	})

	t.Run("fail", func(t *testing.T) {
		l := base()
		l[volumeListingCmd] = fakeListing{stdout: l[volumeListingCmd].stdout + "d\x00900.0\x00950.0\x00./renamed\x00"}
		got, err := verifyVanishedWith(context.Background(), fakeExec(l), archivePath)
		if err != nil || got.missing != 1 || len(got.sample) != 1 || got.sample[0] != "renamed" {
			t.Errorf("Received %+v, %v; wanted 1 missing, renamed", got, err)
		}
	})

	// Every way a listing can go wrong is an error, never a verdict.
	for name, mutate := range map[string]func(map[string]fakeListing){
		"archive does not exist": func(l map[string]fakeListing) {
			l[archiveListingCommand(archivePath)] = fakeListing{code: 2, stderr: archiveDoesNotExistRecord + "\n"}
		},
		"docker fault": func(l map[string]fakeListing) {
			l[volumeListingCmd] = fakeListing{code: 1, err: errors.New("container never came online")}
		},
		"find exits non-zero": func(l map[string]fakeListing) {
			l[volumeListingCmd] = fakeListing{stdout: l[volumeListingCmd].stdout, code: 1, stderr: "find: './x': No such file or directory"}
		},
		"volume listing truncated": func(l map[string]fakeListing) { l[volumeListingCmd] = fakeListing{stdout: "f\x00900.0\x00"} },
		"stamp missing": func(l map[string]fakeListing) {
			l[stampListingCmd] = fakeListing{code: 1, stderr: "find: '/tmp/cs-create-start': No such file or directory"}
		},
		"stamp empty":             func(l map[string]fakeListing) { l[stampListingCmd] = fakeListing{} },
		"tag listing fails":       func(l map[string]fakeListing) { l[cacheTagListingCmd] = fakeListing{code: 1} },
		"archive listing garbled": func(l map[string]fakeListing) { l[archiveListingCommand(archivePath)] = fakeListing{stdout: "{\n"} },
	} {
		t.Run(name, func(t *testing.T) {
			l := base()
			mutate(l)
			if got, err := verifyVanishedWith(context.Background(), fakeExec(l), archivePath); err == nil {
				t.Errorf("Received %+v and no error, wanted an error", got)
			}
		})
	}

	t.Run("borg's record is the diagnosis", func(t *testing.T) {
		l := base()
		l[archiveListingCommand(archivePath)] = fakeListing{code: 2, stderr: archiveDoesNotExistRecord + "\n"}
		_, err := verifyVanishedWith(context.Background(), fakeExec(l), archivePath)
		if err == nil || !strings.Contains(err.Error(), "(Archive.DoesNotExist) Archive does-not-exist does not exist") {
			t.Errorf("Received %v, wanted borg's record in the error", err)
		}
	})
}

// A parser that gives up early must not leave the exec blocked writing into the pipe.
func TestRunListingStopsAnExecWhoseParserFailed(t *testing.T) {
	exec := func(ctx context.Context, cmd []string, w io.Writer) (int, string, error) {
		chunk := bytes.Repeat([]byte("x"), 4096)
		for {
			if _, err := w.Write(chunk); err != nil {
				return 1, "", err
			}
			if ctx.Err() != nil {
				return 1, "", ctx.Err()
			}
		}
	}
	err := runListing(context.Background(), exec, "endless", "yes", func(r io.Reader) error {
		buf := make([]byte, 10)
		_, _ = r.Read(buf)
		return errors.New("parser gave up")
	})
	if err == nil {
		t.Error("Received nil, wanted an error")
	}
}
