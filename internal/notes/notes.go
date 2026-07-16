// Package notes maintains docfetch's machine-written section inside a Homebox
// entity's notes field. The field is shared with free-text the user writes, so
// the machine content lives in one delimited block with explicit start/end
// markers — user text above or below the block survives every rewrite.
//
// Since M2 (D26) the block holds a single breadcrumb line; the full audit
// trail and all portal↔scanner signals live in the SQLite events table.
//
// Rendered form (markers are HTML comments, invisible in markdown):
//
//	<!--docfetch-->
//	#### docfetch
//	- docfetch: manual ✓ · photo ✓ — [log](https://…/log/{id})
//	<!--/docfetch-->
package notes

import (
	"regexp"
	"strings"
)

const (
	Begin  = "<!--docfetch-->"
	End    = "<!--/docfetch-->"
	header = "#### docfetch"
)

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

// Breadcrumb replaces the docfetch block's content with a single status line,
// preserving user text outside the markers byte-for-byte. Homebox caps notes
// length server-side (~1000; an oversized PUT 500s and loses the whole
// payload) — one short line stays far under it by construction. Returns the
// input unchanged when the block already reads exactly this — callers use
// that to skip a no-op PUT (own writes bump updatedAt).
func Breadcrumb(existing, line string) string {
	line = "- " + strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
	block := Begin + "\n" + header + "\n" + line + "\n" + End
	bi := strings.Index(existing, Begin)
	ei := strings.Index(existing, End)
	if bi >= 0 && ei > bi {
		return existing[:bi] + block + existing[ei+len(End):]
	}
	out := strings.TrimRight(existing, "\n")
	if out != "" {
		out += "\n\n"
	}
	return out + block
}
