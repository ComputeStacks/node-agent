package borg

import (
	"encoding/json"
	"strings"
)

// borgLine is one line of a borg response after trimming: the raw text, whatever it
// unmarshalled into, and the two questions callers ask about it.
//
// isJSON and isRecord are deliberately separate. "Not JSON at all" (a shell error,
// wrapper output) IS the diagnosis and is quoted verbatim; "valid JSON that is not a
// log record" is a --json data payload and says nothing about a failure. Collapsing
// the two would dump a compact --json payload into a task's failure reason, which is
// the class of bug that produced a reason of "() ".
type borgLine struct {
	raw      string
	record   LogMessage
	isJSON   bool
	isRecord bool
}

// scanBorgOutput splits a borg response into its lines and parses each one.
//
// Lines are \r\n-framed because Container.Exec allocates a TTY, which also merges
// stderr into stdout. Everything borg writes goes to stderr, so a log record can only
// be told apart from a data payload by its fields — never by which stream carried it.
// Each line is trimmed before parsing.
//
// A line counts as a record only if it unmarshals into LogMessage AND carries a
// non-empty Message or MsgID. borg's --json data payloads unmarshal into LogMessage
// perfectly happily as ALL-ZERO fields, because none of their keys match any of
// LogMessage's, and an all-zero LogMessage treated as a log record is how a failure
// reason once became empty parentheses. Filtering on Type == "log_message" instead
// would look equivalent (every observed record has it) but would silently drop any
// record that did not, and the Message-or-MsgID test already rejects payloads.
func scanBorgOutput(response string) []borgLine {
	var lines []borgLine
	for _, raw := range strings.Split(response, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		l := borgLine{raw: raw}
		if err := json.Unmarshal([]byte(raw), &l.record); err == nil {
			l.isJSON = true
			l.isRecord = l.record.Message != "" || l.record.MsgID != ""
		} else {
			// A failed unmarshal can leave fields partially populated.
			l.record = LogMessage{}
		}
		lines = append(lines, l)
	}
	return lines
}

// failureReason extracts borg's own diagnosis from a --log-json response, dropping
// the `--stats` table (logger name "borg.output.stats"), which explains nothing about
// a failure. Lines that are not valid JSON are kept verbatim — a shell-level error
// (e.g. "sh: borg: not found") is the diagnosis.
//
// Returns "" when borg emitted no diagnosis at all (only --stats output), so the
// caller can say so concisely instead of dumping the stats table as the reason.
func failureReason(response string) string {
	var reasons []string
	for _, l := range scanBorgOutput(response) {
		if !l.isJSON {
			// Not borg JSON at all (shell error, wrapper output) — it is the diagnosis.
			reasons = append(reasons, l.raw)
			continue
		}
		if l.record.Name == "borg.output.stats" {
			continue
		}
		if l.record.Message != "" {
			reasons = append(reasons, l.record.Message)
		}
	}
	return strings.Join(reasons, "; ")
}

// looksLikeJSONFragment reports whether a line that failed to parse as JSON is
// plausibly one line of a pretty-printed --json data payload rather than a
// diagnostic. borg prints --json with indent=4, so a payload's individual lines are
// not valid JSON on their own and would otherwise be mistaken for shell output. Every
// line of a real payload, once trimmed, either starts with one of { } [ ] " or ends
// with a comma. A fragment must never be reported as a failure reason.
func looksLikeJSONFragment(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return true
	}
	if strings.HasSuffix(line, ",") {
		return true
	}
	switch line[0:1] {
	case "{", "}", "[", "]", `"`:
		return true
	}
	return false
}

// looksLikeDiagnosis reports whether a non-JSON line is the one that says what went
// wrong. argparse marks the reason with "<prog>: error: <what>", and the test is
// case-insensitive so Python's own uncaught-exception lines ("...IntegrityError: Data
// integrity error: ...") count too.
//
// "error:" with the colon, not the bare word: "Finished full repository check, errors
// found." and "terminating with error status, rc 2" are summaries that mention an error
// without being one, and matching them would prefer a summary over the cause.
func looksLikeDiagnosis(line string) bool {
	return strings.Contains(strings.ToLower(line), "error:")
}

// severityRank orders borg's levelname values. It decides only WHICH record gets
// quoted, never whether a command failed — borg logs a WARNING on the way to the
// ERROR that actually stopped it, and the ERROR is the useful one. An unrecognised or
// absent level ranks lowest but still qualifies: a record carrying a message beats no
// record at all.
func severityRank(levelName string) int {
	switch strings.ToUpper(strings.TrimSpace(levelName)) {
	case "CRITICAL", "FATAL":
		return 3
	case "ERROR":
		return 2
	case "WARNING", "WARN":
		return 1
	default:
		return 0
	}
}

// failureRecord returns the single record that best explains a failure, VERBATIM —
// Time, Type, Message, MsgID, LevelName and Name exactly as borg wrote them. Verbatim
// is load-bearing: callers branch on MsgID (Repository.DoesNotExist drives
// auto-initialisation of a new volume's repository) and render "(msgid) reason".
//
// Highest severity wins; on a tie a record carrying a MsgID is preferred (cheap
// insurance for the msgid-driven paths); on a further tie the first. Skipped:
// the --stats table, which bypasses borg's level filter and so is often the only
// thing present on a failure while explaining nothing ("Deleted data: 0 B" reads
// like a cause when it is not); and the question protocol records, the same
// exclusion the response parsers already make.
//
// A real record always beats a non-JSON line. A non-JSON line is used only when
// there is no record at all, and then it becomes Message with NO MsgID, so a line
// the agent found rather than borg reported can never be mistaken for borg's own.
//
// Among non-JSON lines the first one that looks like a diagnosis wins, and failing
// that the first usable line. Position alone was measured to be the wrong sole test:
// a rejected argument produces no record at all — argparse writes plain text before
// --log-json takes effect — and it writes the usage banner FIRST and the reason LAST,
// so `--compression zstd,99` reported "usage: borg create [-h] [--critical] [--error]
// ..." when borg had said "borg create: error: argument -C/--compression: level must
// be >= 1 and <= 22". The diagnosis test, not a change of direction, is what fixes
// that: position stays first because when nothing carries "error:" the first line is
// the measured best. `--show-rc` puts "Repository ... does not exist." above
// "terminating with error status, rc 2"; a plain borg traceback puts the exception
// above Platform/PID/sys.argv trailers; multi-line shell failures put the root cause
// above its consequences. In every one of those the later lines follow FROM the first.
//
// How much these two rules have to carry is bounded: every RunBorg command passes
// --log-json, so a borg failure that got as far as running arrives as a record and
// takes the branch above. What reaches here is output produced before borg's json
// logging exists (argparse) or by something other than borg (the shell, a wrapper) —
// short, human-written text, which is why a simple rule is adequate.
//
// ok is false when nothing usable was found.
func failureRecord(response string) (LogMessage, bool) {
	var best LogMessage
	var bestRank int
	var found bool
	var diagnosisLine, firstLine string

	for _, l := range scanBorgOutput(response) {
		if !l.isRecord {
			if !l.isJSON && !looksLikeJSONFragment(l.raw) {
				if firstLine == "" {
					firstLine = l.raw
				}
				if diagnosisLine == "" && looksLikeDiagnosis(l.raw) {
					diagnosisLine = l.raw
				}
			}
			continue
		}
		if l.record.Name == "borg.output.stats" {
			continue
		}
		if l.record.Type == "question_prompt" || l.record.Type == "question_env_answer" {
			continue
		}
		rank := severityRank(l.record.LevelName)
		if !found || rank > bestRank || (rank == bestRank && best.MsgID == "" && l.record.MsgID != "") {
			best, bestRank, found = l.record, rank, true
		}
	}

	if found {
		return best, true
	}
	if diagnosisLine != "" {
		return LogMessage{Message: diagnosisLine}, true
	}
	if firstLine != "" {
		return LogMessage{Message: firstLine}, true
	}
	return LogMessage{}, false
}
