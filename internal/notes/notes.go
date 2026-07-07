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
