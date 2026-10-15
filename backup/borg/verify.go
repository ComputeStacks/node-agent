package borg

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Post-create verification for a `borg create` whose only disqualifying warnings are
// BackupFileNotFoundError: a path that vanished between borg listing its directory and
// stat'ing it.
//
// borg's record cannot say whether that path was deleted (the archive still matches the
// volume) or was a directory renamed mid-walk (its whole subtree is absent from the
// archive and still on the volume) — benignCreateWarnings has the measurements. This
// looks past the record at the two things that can: the committed archive and the volume
// as it stands now.
//
// What a PASS proves, and the wording every message and comment uses: every file and
// directory now on the volume with a timestamp older than the backup's start is in the
// archive at the same path. It does NOT prove that every file that existed before the
// backup started is in the archive. The limits, both directions:
//
//   - Not checked (a lost file can pass): a file renamed mid-walk AND then written, or
//     pre-existing data transformed into a new file mid-walk (`mv log log.1 && gzip
//     log.1`), carries no timestamp older than the start. Directories close most of this
//     for renamed subtrees: a renamed directory keeps its mtime (measured on borg 1.4.4's
//     image: a directory moved to a different parent keeps its mtime, only its ctime
//     moves) even when every file in it is rewritten, unless entries are later added to or
//     removed from it. Also: paths are compared as borg renders them, with every invalid
//     UTF-8 byte as "?", so a non-UTF-8 name lost mid-walk reads as present when the
//     archive holds a literal "?" sibling that renders the same (`caf\351.jpg` beside an
//     archived `caf?.jpg`).
//   - False FAIL (today's behavior, the safe direction): a file written mid-walk with an
//     old mtime (an archive extract, cp -p, rsync -t), and a pre-existing file or
//     directory MOVED after borg walked it — it is on the volume under a path the archive
//     does not have. Both only matter when a vanished-path warning also happened in the
//     same run, because nothing else reaches this code.
//
// Cost: one `borg list` of the archive, one find of the volume and one find for cache
// tags, only on a backup that produced a vanished-path warning. The archive's paths are
// held in memory as a set — O(files) strings for one volume at a time — and the volume
// listing is streamed against it rather than held.

// createStartStamp is the file createCommand touches immediately before borg starts. Its
// mtime is the reference time "older than the backup" is measured against.
//
// A file rather than `date`, deliberately: an inode's timestamps come from the kernel's
// coarse filesystem clock (measured: a ~4ms tick on the image's host), not the
// nanosecond wall clock `date +%s.%N` reads, so a wall-clock start can be LATER than the
// mtime of a file written after it. Stamping a file takes the reference from the same
// clock that stamps the volume's entries. It lives in the container's own filesystem,
// outside /mnt/data, so borg never archives it and the volume listing never sees it.
//
// An entry whose timestamp falls in the same tick as the stamp is not "older" and so is
// not checked. That gap is a few milliseconds wide and sits right before borg starts.
const createStartStamp = "/tmp/cs-create-start"

// maxVerifyReportedPaths caps how many missing paths are kept for the log line.
const maxVerifyReportedPaths = 5

// verifyTimeout bounds the whole verification, so a listing that hangs cannot hold a
// backup worker forever. A timeout fails closed like any other verification error.
const verifyTimeout = 30 * time.Minute

// cacheDirTagSignature is the CACHEDIR.TAG header borg's --exclude-caches checks for:
// borg reads exactly this many bytes and requires them to equal it.
const cacheDirTagSignature = "Signature: 8a477f597d28d172789f06886806bc55"

const (
	msgIDFileNotFound = "BackupFileNotFoundError"
	msgIDFileChanged  = "FileChangedWarning"
)

// wpWriteProbeRE is wpWriteProbeExclude's pattern compiled for the comparison: borg never
// archives what that exclude matches, so the volume side must skip it too. Compiled from
// the same constant so the two cannot drift.
//
// One measured-by-reading difference, and it is in the safe direction: borg matches with
// Python's re.search, whose `$` also matches before a trailing newline. A probe-shaped
// name ending in "\n" is excluded by borg and not skipped here, so it reads as missing —
// a false FAIL, never a false PASS.
var wpWriteProbeRE = regexp.MustCompile(wpWriteProbePattern)

// Listing commands, each its own exec so its exit code reflects only the listing it
// produced. None is a pipeline: a failing earlier stage could otherwise be masked by a
// later one exiting 0.
const (
	// The stamp's mtime, in the same format and from the same tool as the volume listing.
	stampListingCmd = "find " + createStartStamp + ` -maxdepth 0 -printf '%T@\0'`

	// One NUL-terminated record of four fields per entry: type, mtime, ctime, path. NUL
	// is the only byte a path cannot contain. -xdev mirrors create's --one-file-system.
	//
	// find exits non-zero if an entry vanishes between its own readdir and stat, which
	// fails the verification closed. That is deliberate: -ignore_readdir_race would hide
	// an entry renamed during the listing — the one case where the volume listing could
	// miss something the archive is also missing.
	volumeListingCmd = `cd /mnt/data && find . -xdev -printf '%y\0%T@\0%C@\0%p\0'`

	// The directories borg's --exclude-caches dropped: one NUL-terminated %h per regular
	// CACHEDIR.TAG whose first 43 bytes are exactly the signature (head -c bounds the read
	// to the signature's length, so command substitution's trailing-newline strip cannot
	// make a short file compare equal). The test is a find predicate: if it errors, the
	// tag is simply not reported and the directory's contents are compared like any
	// other — a FAIL, never a PASS, if borg did exclude it.
	cacheTagListingCmd = `cd /mnt/data && find . -xdev -type f -name CACHEDIR.TAG ` +
		`-exec sh -c '[ "$(head -c 43 "$1")" = "` + cacheDirTagSignature + `" ]' sh {} \; -printf '%h\0'`
)

// archiveListingCommand lists every item in a committed archive, one JSON object per line.
//
// --bypass-lock exactly as Archive.Info uses it: the listing is read-only on an archive
// that is already committed, and a prune queued behind the create must not be able to
// make it time out.
func archiveListingCommand(archivePath string) string {
	return "borg --log-json list --bypass-lock --json-lines " + archivePath
}

// streamExec runs a command in the backup container, streaming stdout to w — the shape of
// containermgr.Container.ExecStream, which is TTY-free and therefore binary-safe.
type streamExec func(ctx context.Context, cmd []string, stdout io.Writer) (exitCode int, stderr string, err error)

// verifiableVanishedCreate reports whether a create that wroteCompleteArchive refused may
// be settled by verification instead of failed outright.
//
// The terms mirror wroteCompleteArchive's — borg's own warning tier, an archive borg says
// it committed — and narrow the records to the two this verification can rule on: at least
// one BackupFileNotFoundError, and otherwise only FileChangedWarning, which is already
// benign. Any other record (BackupPermissionError and its siblings) is a loss the
// verification has no business excusing, so it fails exactly as before.
func verifiableVanishedCreate(res ExecResult, decoded bool, archiveID string) bool {
	if res.DockerFault || res.ExitCode != borgWarningExit {
		return false
	}
	if !decoded || archiveID == "" {
		return false
	}
	vanished := 0
	for _, w := range createWarnings(res.Response) {
		switch w.MsgID {
		case msgIDFileNotFound:
			vanished++
		case msgIDFileChanged:
		default:
			return false
		}
	}
	return vanished > 0
}

// verifyResult is the verification's verdict: how many entries are on the volume, older
// than the backup's start, and absent from the archive at that path, plus the first few of
// them in sorted order, rendered safe for a log line or result_json.
type verifyResult struct {
	missing int
	sample  []string
}

// verifyVanished runs the verification against this archive in its backup container.
func (a *Archive) verifyVanished() (verifyResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), verifyTimeout)
	defer cancel()
	return verifyVanishedWith(ctx, a.Repository.Container.ExecStream, a.archivePath())
}

// verifyVanishedWith is the verification with its exec injected. Any error means no
// verdict: the caller fails closed.
//
// Order matters only for memory: the archive set must exist before the volume listing
// streams against it.
func verifyVanishedWith(ctx context.Context, exec streamExec, archivePath string) (verifyResult, error) {
	var start fsTimestamp
	if err := runListing(ctx, exec, "start stamp", stampListingCmd, func(r io.Reader) error {
		var err error
		start, err = parseStamp(r)
		return err
	}); err != nil {
		return verifyResult{}, err
	}

	var archive map[string]struct{}
	if err := runListing(ctx, exec, "borg list", archiveListingCommand(archivePath), func(r io.Reader) error {
		var err error
		archive, err = parseArchiveListing(r)
		return err
	}); err != nil {
		return verifyResult{}, err
	}

	var cacheDirs []string
	if err := runListing(ctx, exec, "cache tag listing", cacheTagListingCmd, func(r io.Reader) error {
		var err error
		cacheDirs, err = parseNULList(r)
		return err
	}); err != nil {
		return verifyResult{}, err
	}

	cmp := newVolumeComparison(start, archive, cacheDirs)
	if err := runListing(ctx, exec, "volume listing", volumeListingCmd, func(r io.Reader) error {
		return parseVolumeListing(r, cmp.add)
	}); err != nil {
		return verifyResult{}, err
	}
	return cmp.result(), nil
}

// runListing runs one listing command and streams its stdout into parse.
//
// The exec's own verdict outranks the parser's: a docker fault or a non-zero exit is
// reported as such even when it also left the parser a truncated stream. A parser that
// stops early cancels the exec rather than leaving it blocked on a full pipe.
func runListing(ctx context.Context, exec streamExec, label, cmd string, parse func(io.Reader) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type outcome struct {
		code   int
		stderr string
		err    error
	}
	pr, pw := io.Pipe()
	done := make(chan outcome, 1)
	go func() {
		code, stderr, err := exec(ctx, []string{"sh", "-c", cmd}, pw)
		_ = pw.Close()
		done <- outcome{code, stderr, err}
	}()

	parseErr := parse(pr)
	if parseErr != nil {
		cancel()
	}
	_ = pr.Close()
	o := <-done

	if o.err != nil {
		return fmt.Errorf("%s: %w", label, o.err)
	}
	if o.code != 0 {
		return fmt.Errorf("%s exited %d: %s", label, o.code, listingDiagnosis(o.stderr))
	}
	if parseErr != nil {
		return fmt.Errorf("%s: %w", label, parseErr)
	}
	return nil
}

// listingDiagnosis is the reason a listing gave for failing: borg's own record when it
// wrote one (the archive listing runs with --log-json), otherwise its stderr, capped.
func listingDiagnosis(stderr string) string {
	if record, ok := failureRecord(stderr); ok {
		if record.MsgID != "" {
			return truncateReason("(" + record.MsgID + ") " + record.Message)
		}
		return truncateReason(record.Message)
	}
	if s := strings.TrimSpace(stderr); s != "" {
		return truncateReason(s)
	}
	return "no diagnostic output"
}

// fsTimestamp is an inode timestamp exactly as the kernel holds it — whole seconds and
// nanoseconds — so two of them compare without float rounding. A float64 cannot hold a
// 2026 epoch to the nanosecond.
type fsTimestamp struct {
	sec  int64
	nsec int64
}

func (t fsTimestamp) before(u fsTimestamp) bool {
	return t.sec < u.sec || (t.sec == u.sec && t.nsec < u.nsec)
}

// parseFindTimestamp parses GNU find's %T@/%C@ rendering.
//
// Measured on the image's findutils: ten fractional digits, the tenth always 0, and the
// sign belongs to the seconds only — tv_sec and tv_nsec printed side by side. 1.5 seconds
// before the epoch prints as "-2.5000000000" (tv_sec -2, tv_nsec 500000000), and one
// nanosecond before it as "-1.9999999990". So the integer part is tv_sec and the fraction
// is tv_nsec, for either sign.
//
// Anything else is an error rather than a guess: a digit past the ninth that is not 0
// would be precision this comparison cannot represent.
func parseFindTimestamp(s string) (fsTimestamp, error) {
	intPart, frac, ok := strings.Cut(s, ".")
	if !ok || intPart == "" || intPart == "-" || frac == "" {
		return fsTimestamp{}, fmt.Errorf("malformed timestamp %q", s)
	}
	sec, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil || intPart[0] == '+' {
		return fsTimestamp{}, fmt.Errorf("malformed timestamp %q", s)
	}
	for i := 0; i < len(frac); i++ {
		if frac[i] < '0' || frac[i] > '9' {
			return fsTimestamp{}, fmt.Errorf("malformed timestamp %q", s)
		}
		if i >= 9 && frac[i] != '0' {
			return fsTimestamp{}, fmt.Errorf("timestamp %q has precision finer than a nanosecond", s)
		}
	}
	if len(frac) > 9 {
		frac = frac[:9]
	}
	frac += strings.Repeat("0", 9-len(frac))
	nsec, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return fsTimestamp{}, fmt.Errorf("malformed timestamp %q", s)
	}
	return fsTimestamp{sec: sec, nsec: nsec}, nil
}

// parseStamp reads stampListingCmd's output: exactly one NUL-terminated timestamp.
func parseStamp(r io.Reader) (fsTimestamp, error) {
	fields, err := parseNULList(r)
	if err != nil {
		return fsTimestamp{}, err
	}
	if len(fields) != 1 {
		return fsTimestamp{}, fmt.Errorf("start stamp: wanted one timestamp, got %d", len(fields))
	}
	return parseFindTimestamp(fields[0])
}

// parseNULList reads NUL-terminated fields to EOF. A final field with no terminator is a
// truncated stream and an error.
func parseNULList(r io.Reader) ([]string, error) {
	br := bufio.NewReader(r)
	var out []string
	for {
		field, err := br.ReadString(0)
		if err == io.EOF {
			if field != "" {
				return nil, errors.New("truncated listing: last field has no NUL terminator")
			}
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, field[:len(field)-1])
	}
}

// volumeEntry is one record of volumeListingCmd. path is find's, raw bytes and all,
// with its leading "./".
type volumeEntry struct {
	typ   byte
	mtime fsTimestamp
	ctime fsTimestamp
	path  string
}

// parseVolumeListing streams volumeListingCmd's records into each. A record cut short —
// fewer than four NUL-terminated fields at EOF — is an error, so a listing that stopped
// part-way is never mistaken for a short volume.
func parseVolumeListing(r io.Reader, each func(volumeEntry)) error {
	br := bufio.NewReader(r)
	var fields [4]string
	for {
		for i := range fields {
			field, err := br.ReadString(0)
			if err == io.EOF {
				if i == 0 && field == "" {
					return nil
				}
				return errors.New("truncated volume listing: incomplete record at end of stream")
			}
			if err != nil {
				return err
			}
			fields[i] = field[:len(field)-1]
		}
		if len(fields[0]) != 1 {
			return fmt.Errorf("malformed volume listing: type %q", fields[0])
		}
		mtime, err := parseFindTimestamp(fields[1])
		if err != nil {
			return err
		}
		ctime, err := parseFindTimestamp(fields[2])
		if err != nil {
			return err
		}
		each(volumeEntry{typ: fields[0][0], mtime: mtime, ctime: ctime, path: fields[3]})
	}
}

// maxArchiveLineBytes bounds one `borg list --json-lines` line. A path is at most PATH_MAX
// (4096) bytes, and JSON's \uXXXX escaping expands a byte at most sixfold; the rest of a
// line is well under a kilobyte.
const maxArchiveLineBytes = 64 * 1024

// parseArchiveListing reads `borg list --json-lines` into the set of archived paths.
//
// The paths are taken exactly as borg renders them, which is not always the bytes on
// disk: borg replaces every byte of a filename that is not valid UTF-8 with "?"
// (measured on 1.4.4 — see normalizeBorgPath). The volume side is normalized to match.
//
// Every non-empty line must be a JSON object carrying a path; anything else is an error,
// not a line to skip, because a skipped line would read as a missing path or — worse — a
// set that silently came out short.
func parseArchiveListing(r io.Reader) (map[string]struct{}, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxArchiveLineBytes)
	paths := make(map[string]struct{})
	line := 0
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var item struct {
			Path *string `json:"path"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("archive listing line %d: %w", line, err)
		}
		if item.Path == nil {
			return nil, fmt.Errorf("archive listing line %d: no path", line)
		}
		paths[*item.Path] = struct{}{}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("archive listing: %w", err)
	}
	return paths, nil
}

// normalizeBorgPath renders a raw filename the way `borg list` reports it: every byte that
// is not part of a valid UTF-8 sequence becomes "?", ONE PER BYTE.
//
// Per byte, not per run, which is why this is not strings.ToValidUTF8: measured on borg
// 1.4.4, "caf\351.jpg" lists as "caf?.jpg", "a\351\352b" as "a??b", and a truncated
// three-byte sequence "tr\342\202x" as "tr??x". borg decodes with surrogateescape (one
// surrogate per undecodable byte) and lists with each surrogate replaced; utf8.DecodeRune
// likewise reports an invalid sequence one byte at a time.
func normalizeBorgPath(p string) string {
	if utf8.ValidString(p) {
		return p
	}
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); {
		r, size := utf8.DecodeRuneInString(p[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteByte('?')
		} else {
			b.WriteString(p[i : i+size])
		}
		i += size
	}
	return b.String()
}

// safePath renders a path for a log line or result_json: valid UTF-8 (see
// normalizeBorgPath), with every non-printable character — a newline in a filename above
// all — escaped Go-style rather than passed through.
func safePath(p string) string {
	p = normalizeBorgPath(p)
	var b strings.Builder
	for _, r := range p {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
			continue
		}
		q := strconv.QuoteRune(r)
		b.WriteString(q[1 : len(q)-1])
	}
	return b.String()
}

// volumeComparison checks volume entries against the archive one at a time, so the
// volume listing can be streamed rather than held.
type volumeComparison struct {
	start     fsTimestamp
	archive   map[string]struct{}
	cacheDirs map[string]struct{}
	missing   int
	sample    []string // the smallest missing paths so far, sorted, at most maxVerifyReportedPaths
}

// newVolumeComparison prepares a comparison. cacheDirs are find's %h values for the
// directories borg's --exclude-caches dropped ("." for the volume root).
func newVolumeComparison(start fsTimestamp, archive map[string]struct{}, cacheDirs []string) *volumeComparison {
	dirs := make(map[string]struct{}, len(cacheDirs))
	for _, d := range cacheDirs {
		dirs[stripDotSlash(d)] = struct{}{}
	}
	return &volumeComparison{start: start, archive: archive, cacheDirs: dirs}
}

// stripDotSlash turns find's "./a/b" into borg's "a/b", and the root "." into "".
func stripDotSlash(p string) string {
	if p == "." {
		return ""
	}
	return strings.TrimPrefix(p, "./")
}

// add rules on one volume entry. It is counted as missing when ALL of:
//
//   - it is a regular file, symlink or directory — what borg archives as content. Sockets,
//     fifos and devices are skipped: nothing is lost if one is not in the archive.
//   - it is not the volume root, which is the walk's starting point, not an entry.
//   - it is not inside something borg was told to leave out: a directory with a valid
//     CACHEDIR.TAG (borg drops the directory entry itself as well as everything under it,
//     measured), or a path matching the WordPress write-probe exclude — or under one, since
//     borg prunes a matching directory's subtree.
//   - its mtime OR its ctime is strictly older than the start stamp. Either one dates the
//     entry to before the backup: a file renamed mid-walk keeps its old mtime while its
//     ctime moves, and a file whose mtime was set into the past still has a new ctime.
//   - its path, rendered the way borg renders it, is not in the archive.
func (c *volumeComparison) add(e volumeEntry) {
	if e.typ != 'f' && e.typ != 'l' && e.typ != 'd' {
		return
	}
	p := stripDotSlash(e.path)
	if p == "" {
		return
	}
	if c.excluded(p) {
		return
	}
	if !e.mtime.before(c.start) && !e.ctime.before(c.start) {
		return
	}
	if _, ok := c.archive[normalizeBorgPath(p)]; ok {
		return
	}
	c.missing++
	c.keepSample(p)
}

// excluded reports whether p, or any directory above it, is something borg excluded.
func (c *volumeComparison) excluded(p string) bool {
	if _, ok := c.cacheDirs[""]; ok {
		return true
	}
	for q := p; ; {
		if _, ok := c.cacheDirs[q]; ok {
			return true
		}
		if wpWriteProbeRE.MatchString(q) {
			return true
		}
		i := strings.LastIndexByte(q, '/')
		if i < 0 {
			return false
		}
		q = q[:i]
	}
}

func (c *volumeComparison) keepSample(p string) {
	i := sort.SearchStrings(c.sample, p)
	if i >= maxVerifyReportedPaths {
		return
	}
	c.sample = append(c.sample, "")
	copy(c.sample[i+1:], c.sample[i:])
	c.sample[i] = p
	if len(c.sample) > maxVerifyReportedPaths {
		c.sample = c.sample[:maxVerifyReportedPaths]
	}
}

func (c *volumeComparison) result() verifyResult {
	sample := make([]string, len(c.sample))
	for i, p := range c.sample {
		sample[i] = safePath(p)
	}
	return verifyResult{missing: c.missing, sample: sample}
}

// compareVolumeToArchive is the whole comparison over an in-memory volume listing: the
// number of entries on the volume older than start and absent from the archive at that
// path, and the first maxVerifyReportedPaths of them in sorted order, rendered safe.
func compareVolumeToArchive(vol []volumeEntry, start fsTimestamp, archive map[string]struct{}, cacheDirs []string) (missing int, sample []string) {
	c := newVolumeComparison(start, archive, cacheDirs)
	for _, e := range vol {
		c.add(e)
	}
	r := c.result()
	return r.missing, r.sample
}

// vanishedRecords counts a create's BackupFileNotFoundError and FileChangedWarning records
// and returns the first of the former.
func vanishedRecords(response string) (first *LogMessage, vanished, changed int) {
	for _, w := range createWarnings(response) {
		switch w.MsgID {
		case msgIDFileNotFound:
			if first == nil {
				rec := w
				first = &rec
			}
			vanished++
		case msgIDFileChanged:
			changed++
		}
	}
	return first, vanished, changed
}

// vanishedPath extracts the path from a BackupFileNotFoundError message, which borg writes
// as "<path>: stat: [Errno 2] No such file or directory: '<name>'". Falls back to the whole
// message if it is not in that shape.
func vanishedPath(message string) string {
	if i := strings.Index(message, ": stat: "); i > 0 {
		return message[:i]
	}
	return message
}

// vanishedWarning is the backup_warning for a create the verification passed. It keeps
// borg's msgid and record fields, and replaces the message with one that says what was
// verified — in the narrowed wording, not "nothing was lost".
func vanishedWarning(res ExecResult) *LogMessage {
	first, vanished, changed := vanishedRecords(res.Response)
	warning := LogMessage{MsgID: msgIDFileNotFound, LevelName: "WARNING"}
	firstPath := ""
	if first != nil {
		warning = *first
		firstPath = safePath(vanishedPath(first.Message))
	}
	msg := strconv.Itoa(vanished) + " path(s) vanished during the backup (first: " + firstPath +
		"); every file and directory on the volume older than the backup is in the archive"
	if changed > 0 {
		// The torn-read signal FileChangedWarning would have carried on its own.
		msg += "; " + strconv.Itoa(changed) + " file(s) changed while being read"
	}
	warning.Message = truncateReason(msg)
	return &warning
}

// verificationFailure is the reason for a create the verification failed, logged on the
// same "Backup failed" line createFailure writes. "Not in the archive at that path"
// rather than "lost": a pre-existing entry moved after borg walked it also lands here.
func (a *Archive) verificationFailure(res ExecResult, v verifyResult) *LogMessage {
	first, _, _ := vanishedRecords(res.Response)
	failure := LogMessage{MsgID: msgIDFileNotFound, LevelName: "WARNING"}
	if first != nil {
		failure = *first
	}
	firstPath := ""
	if len(v.sample) > 0 {
		firstPath = v.sample[0]
	}
	failure.Message = truncateReason(strconv.Itoa(v.missing) +
		" path(s) on the volume older than the backup are not in the archive at that path (first: " + firstPath + ")")
	borgLogger().Warn("Backup failed", "repo", a.Repository.Name, "exitCode", res.ExitCode,
		"msgid", failure.MsgID, "reason", failure.Message, "missingSample", strings.Join(v.sample, ", "))
	return &failure
}
