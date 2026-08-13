package portal

import (
	"fmt"
	"html"
	"strings"
)

// The server-rendered pages (activity log, ntfy action confirmation) use the
// same shell as the intake app: same stylesheet, same header row with the
// theme control, same bottom action bar. They open in the browser rather than
// the installed app, but they are the same origin, so app.js picks up the
// theme the user chose in the app and falls back to the OS.

const shellHead = `<!doctype html><meta charset="utf-8">` +
	`<meta name="viewport" content="width=device-width, initial-scale=1, maximum-scale=1, user-scalable=no, viewport-fit=cover">` +
	`<meta name="theme-color" content="#111318">` +
	`<link rel="stylesheet" href="/app.css"><script src="/app.js"></script>`

// Monochrome silhouettes, matching the set in index.html and app.js.
const (
	iconBox  = `<svg viewBox="0 0 24 24" aria-hidden="true"><path fill-rule="evenodd" d="M12 1.5l9.5 4.8v11.4L12 22.5 2.5 17.7V6.3L12 1.5zM4.5 8v8.4l6.5 3.3v-8.4L4.5 8zm15 0L13 11.3v8.4l6.5-3.3V8zm-2.2-1.6L12 3.7 6.7 6.4 12 9l5.3-2.6z"/></svg>`
	iconList = `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 5h2v2H3V5zm4 0h14v2H7V5zM3 11h2v2H3v-2zm4 0h14v2H7v-2zM3 17h2v2H3v-2zm4 0h14v2H7v-2z"/></svg>`
	iconChk  = `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9.6 16.7L4.9 12l-1.4 1.4 6.1 6.1L20.6 8.6 19.2 7.2z"/></svg>`
)

// action is one button in the bottom bar. Exactly one should be primary.
type action struct {
	href, icon, label string
	primary           bool
}

// shellOpen writes everything up to the page's own content: the screen, the
// scrolling body, and the header row carrying the title and theme control.
// fill makes the body a flex column so the content stretches to the bottom and
// scrolls inside its own frame — what the log wants, so its header row stays
// put and only the table moves.
func shellOpen(b *strings.Builder, title string, fill bool) {
	b.WriteString(shellHead)
	fmt.Fprintf(b, `<title>%s</title>`, html.EscapeString(title))
	body := "sbody"
	if fill {
		body += " grow"
	}
	fmt.Fprintf(b, `<div class="screen active"><div class="%s">`+
		`<div class="shead"><h1>%s</h1>`+
		`<button class="themebtn" onclick="cycleTheme()"></button></div>`,
		body, html.EscapeString(title))
}

// shellClose ends the body and writes the action bar.
func shellClose(b *strings.Builder, acts ...action) {
	b.WriteString(`</div><div class="actions"><div class="btnrow">`)
	for _, a := range acts {
		kind := "secondary"
		if a.primary {
			kind = "primary"
		}
		fmt.Fprintf(b, `<a class="btn %s" href="%s">%s<span class="lbl">%s</span></a>`,
			kind, html.EscapeString(a.href), a.icon, html.EscapeString(a.label))
	}
	b.WriteString(`</div></div></div>`)
}
