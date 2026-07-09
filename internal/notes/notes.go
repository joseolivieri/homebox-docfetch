// Package notes maintains docfetch's machine-written section inside a Homebox
// entity's notes field. The field is shared with free-text the user writes, so
// all machine lines live in one delimited block with explicit start/end
// markers — user text above or below the block survives every append.
//
// Rendered form (markers are HTML comments, invisible in markdown):
//
//	<!--docfetch-->
//	#### docfetch
//	- 2026-07-07 created via photo intake
//	- 2026-07-07 official photo (conf 0.95) — example.com/img.jpg
//	<!--/docfetch-->
package notes

import (
	"regexp"
	"strings"
	"time"
)

const (
	Begin  = "<!--docfetch-->"
	End    = "<!--/docfetch-->"
	header = "#### docfetch"
)

// Line formats one compact log entry: "- YYYY-MM-DD <text>".
func Line(text string) string {
	return "- " + time.Now().Format("2006-01-02") + " " + strings.TrimSpace(text)
}

// MDLink renders a short-labeled markdown link — raw URLs are noisy in the
// Homebox UI, so every logged/linked URL gets a label like [pdf](…) or [web](…).
func MDLink(label, url string) string {
	return "[" + label + "](" + url + ")"
}

var mdTarget = regexp.MustCompile(`\]\((https?://[^)\s]+)\)`)
var bareURL = regexp.MustCompile(`https?://\S+`)

// Target extracts the URL from a value that is either a markdown link
// ("[pdf](https://…)") or a bare URL. "" when neither.
func Target(s string) string {
	if m := mdTarget.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return strings.TrimRight(bareURL.FindString(s), ".,;")
}

// RejectedURLs extracts doc URLs from "rejected" log lines inside the docfetch
// block. These lines are written by the ntfy Reject button (via the portal) or
// by hand, and act as durable negative labels: the scanner must never propose
// these URLs again. Both [label](url) and bare-URL forms are recognized.
func RejectedURLs(existing string) []string { return markedURLs(existing, "rejected") }

// ApprovedURLs extracts doc URLs from "approved" log lines. Written by the
// ntfy Attach button (via the portal): the portal makes no web calls, so
// approval is queued through the notes block and the scanner downloads and
// attaches on its next pass (~30s via the change-poll).
func ApprovedURLs(existing string) []string { return markedURLs(existing, "approved") }

// QRURLs extracts URLs from "qr" log lines — support links decoded from QR
// codes on the item's physical labels at intake. Manufacturer-printed
// provenance: the scanner's "qr" pipeline stage tries these before searching.
func QRURLs(existing string) []string { return markedURLs(existing, "qr") }

// markedURLs scans docfetch-block log lines whose first word (after the date)
// is the keyword and returns their URL targets. First-token matching, not
// substring: "qr" must not match URLs like qr.anker.com on other lines, and
// "approved" must not match "manual attached via approve".
func markedURLs(existing, keyword string) []string {
	bi := strings.Index(existing, Begin)
	ei := strings.Index(existing, End)
	if bi < 0 || ei <= bi {
		return nil
	}
	var out []string
	for _, line := range strings.Split(existing[bi:ei], "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "-") {
			continue
		}
		f := strings.Fields(l)
		// Expected shape: "- YYYY-MM-DD <keyword> ..."; tolerate a missing
		// date for hand-written lines ("- rejected <url>").
		kw := ""
		if len(f) >= 3 && dateLike(f[1]) {
			kw = f[2]
		} else if len(f) >= 2 {
			kw = f[1]
		}
		if kw != keyword {
			continue
		}
		if m := mdTarget.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
		} else if u := bareURL.FindString(l); u != "" {
			out = append(out, strings.TrimRight(u, ".,;"))
		}
	}
	return out
}

func dateLike(s string) bool {
	return len(s) == 10 && s[4] == '-' && s[7] == '-'
}

// MaxNotes is the byte budget for the whole notes value. Homebox validates
// notes length server-side (observed live: a long audit block turned every
// PUT into a 500, losing the fields written alongside it); we prune our own
// block to stay under rather than let the whole update bounce.
const MaxNotes = 1000

// Append inserts log lines into the docfetch block of an existing notes value,
// creating the block (after any user text) when absent. User content outside
// the markers is preserved byte-for-byte. The result is pruned to MaxNotes:
// oldest plain log lines go first; semantic lines (qr/rejected/approved — they
// are machine-readable state, not audit) are dropped only as a last resort,
// oldest first. The full audit trail lives in the ledger regardless.
func Append(existing string, lines ...string) string {
	if len(lines) == 0 {
		return existing
	}
	block := strings.Join(lines, "\n")

	bi := strings.Index(existing, Begin)
	ei := strings.Index(existing, End)
	var out string
	if bi >= 0 && ei > bi {
		// Insert just before the end marker, keeping surrounding text intact.
		before := strings.TrimRight(existing[:ei], "\n")
		after := existing[ei:]
		out = before + "\n" + block + "\n" + after
	} else {
		out = strings.TrimRight(existing, "\n")
		if out != "" {
			out += "\n\n"
		}
		out += Begin + "\n" + header + "\n" + block + "\n" + End
	}
	return prune(out, MaxNotes)
}

// semanticLine reports whether a block line carries machine-readable state
// that the scanner parses back (see markedURLs) rather than audit history.
func semanticLine(l string) bool {
	f := strings.Fields(strings.TrimSpace(l))
	kw := ""
	if len(f) >= 3 && dateLike(f[1]) {
		kw = f[2]
	} else if len(f) >= 2 {
		kw = f[1]
	}
	return kw == "qr" || kw == "rejected" || kw == "approved"
}

// prune drops docfetch-block lines (never user text) until the whole notes
// value fits the budget: plain audit lines first, semantic lines last resort,
// both oldest-first.
func prune(notes string, max int) string {
	if len(notes) <= max {
		return notes
	}
	bi := strings.Index(notes, Begin)
	ei := strings.Index(notes, End)
	if bi < 0 || ei <= bi {
		return notes // no block of ours to shrink
	}
	head := notes[:bi]
	tail := notes[ei+len(End):]
	inner := strings.Split(strings.Trim(notes[bi+len(Begin):ei], "\n"), "\n")
	rebuild := func() string {
		return head + Begin + "\n" + strings.Join(inner, "\n") + "\n" + End + tail
	}
	for _, dropSemantic := range []bool{false, true} {
		for len(rebuild()) > max {
			idx := -1
			for i, l := range inner {
				t := strings.TrimSpace(l)
				if !strings.HasPrefix(t, "-") {
					continue // header line
				}
				if !dropSemantic && semanticLine(t) {
					continue
				}
				idx = i
				break
			}
			if idx < 0 {
				break // nothing droppable in this pass
			}
			inner = append(inner[:idx], inner[idx+1:]...)
		}
	}
	return rebuild()
}
