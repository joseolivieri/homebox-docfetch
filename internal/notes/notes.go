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

// Append inserts log lines into the docfetch block of an existing notes value,
// creating the block (after any user text) when absent. User content outside
// the markers is preserved byte-for-byte.
func Append(existing string, lines ...string) string {
	if len(lines) == 0 {
		return existing
	}
	block := strings.Join(lines, "\n")

	bi := strings.Index(existing, Begin)
	ei := strings.Index(existing, End)
	if bi >= 0 && ei > bi {
		// Insert just before the end marker, keeping surrounding text intact.
		before := strings.TrimRight(existing[:ei], "\n")
		after := existing[ei:]
		return before + "\n" + block + "\n" + after
	}

	// No block yet: append one at the end.
	out := strings.TrimRight(existing, "\n")
	if out != "" {
		out += "\n\n"
	}
	return out + Begin + "\n" + header + "\n" + block + "\n" + End
}
