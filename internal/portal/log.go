package portal

import (
	"fmt"
	"html"
	"net/http"
	"strings"
)

// handleLog renders the activity log: /log (recent events across all items)
// and /log/{entityID} (full per-item history). Server-rendered HTML, no JS —
// the "instructionally cheap" surface for the events table (M2).
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	entityID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/log"), "/")
	limit := 100
	if entityID != "" {
		limit = 500
	}
	events, err := s.st.Events(r.Context(), entityID, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	title := "docfetch activity"
	if entityID != "" {
		title = "docfetch activity — "
		if len(events) > 0 && events[0].EntityName != "" {
			title += events[0].EntityName
		} else {
			title += entityID
		}
	}

	var b strings.Builder
	b.WriteString(`<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><meta charset="utf-8">`)
	fmt.Fprintf(&b, `<title>%s</title>`, html.EscapeString(title))
	b.WriteString(`<style>
body{font-family:system-ui;background:#111318;color:#e6e6e9;margin:1rem;font-size:.9rem}
h1{font-size:1.1rem}
table{border-collapse:collapse;width:100%}
td,th{padding:.35rem .6rem;border-bottom:1px solid #2a2d35;text-align:left;vertical-align:top}
th{color:#9aa0ac;font-weight:600}
a{color:#7aa2f7;text-decoration:none;word-break:break-all}
.kind{white-space:nowrap;color:#c3e88d}
.ts{white-space:nowrap;color:#9aa0ac}
.actor{color:#9aa0ac}
</style>`)
	fmt.Fprintf(&b, `<h1>%s</h1>`, html.EscapeString(title))
	if entityID != "" {
		b.WriteString(`<p><a href="/log">&larr; all items</a></p>`)
	}
	b.WriteString(`<table><tr><th>when</th>`)
	if entityID == "" {
		b.WriteString(`<th>item</th>`)
	}
	b.WriteString(`<th>event</th><th>detail</th></tr>`)
	for _, e := range events {
		b.WriteString(`<tr>`)
		fmt.Fprintf(&b, `<td class="ts">%s</td>`, e.Ts.Format("2006-01-02 15:04"))
		if entityID == "" {
			name := e.EntityName
			if name == "" {
				name = e.EntityID
			}
			fmt.Fprintf(&b, `<td><a href="/log/%s">%s</a></td>`,
				html.EscapeString(e.EntityID), html.EscapeString(name))
		}
		kind := e.Kind
		if e.Class != "" && e.Class != e.Kind {
			kind += " · " + e.Class
		}
		fmt.Fprintf(&b, `<td class="kind">%s <span class="actor">(%s)</span></td>`,
			html.EscapeString(kind), html.EscapeString(e.Actor))
		detail := html.EscapeString(e.Detail)
		if e.URL != "" {
			detail += fmt.Sprintf(` <a href="%s">%s</a>`,
				html.EscapeString(e.URL), html.EscapeString(shortURL(e.URL)))
		}
		fmt.Fprintf(&b, `<td>%s</td>`, detail)
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</table>`)
	if len(events) == 0 {
		b.WriteString(`<p>no events yet</p>`)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// shortURL abbreviates a URL for display: host + trailing path element.
func shortURL(u string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if len(trimmed) <= 60 {
		return trimmed
	}
	if i := strings.Index(trimmed, "/"); i > 0 {
		host := trimmed[:i]
		if j := strings.LastIndex(trimmed, "/"); j > i {
			return host + "/…/" + trimmed[j+1:]
		}
	}
	return trimmed[:57] + "…"
}
