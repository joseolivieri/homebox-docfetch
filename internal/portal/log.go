package portal

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/joseolivieri/homebox-docfetch/internal/store"
)

// handleLog renders the activity log: /log (recent events across all items)
// and /log/{entityID} (full per-item history). Server-rendered HTML, no JS —
// the "instructionally cheap" surface for the events table (M2).
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Intake.LiveLogEnabled() {
		http.NotFound(w, r)
		return
	}
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
		title = entityID
		if len(events) > 0 && events[0].EntityName != "" {
			title = events[0].EntityName
		}
	}

	// Same shell as the intake app: header row with the theme control, a
	// scrolling body, and the action bar pinned to the bottom.
	var b strings.Builder
	shellOpen(&b, title, true)
	if entityID != "" {
		s.writeFacts(r.Context(), &b, entityID)
	}
	b.WriteString(`<div class="logbox"><table class="logtbl"><thead><tr><th>when</th>`)
	if entityID == "" {
		b.WriteString(`<th>item</th>`)
	}
	b.WriteString(`<th>event</th><th>detail</th></tr></thead><tbody>`)
	for _, e := range events {
		b.WriteString(`<tr>`)
		fmt.Fprintf(&b, `<td class="lt">%s</td>`, e.Ts.Local().Format("01-02 15:04"))
		if entityID == "" {
			name := e.EntityName
			if name == "" {
				name = e.EntityID
			}
			fmt.Fprintf(&b, `<td><a href="/log/%s">%s</a></td>`,
				html.EscapeString(e.EntityID), html.EscapeString(name))
		}
		fmt.Fprintf(&b, `<td class="lk">%s`, html.EscapeString(e.Kind))
		if e.Class != "" && e.Class != e.Kind {
			fmt.Fprintf(&b, ` <span class="lc">%s</span>`, html.EscapeString(e.Class))
		}
		fmt.Fprintf(&b, ` <span class="lt">(%s)</span></td>`, html.EscapeString(e.Actor))
		detail := html.EscapeString(e.Detail)
		if e.URL != "" {
			detail += fmt.Sprintf(` <a href="%s">%s</a>`,
				html.EscapeString(e.URL), html.EscapeString(shortURL(e.URL)))
		}
		fmt.Fprintf(&b, `<td class="ld">%s</td>`, detail)
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table>`)
	if len(events) == 0 {
		b.WriteString(`<div class="logempty">no events yet</div>`)
	}
	b.WriteString(`</div>`)

	acts := []action{{href: "/", icon: iconBox, label: "Intake", primary: true}}
	if entityID != "" {
		acts = append([]action{{href: "/log", icon: iconList, label: "All items"}}, acts...)
	}
	shellClose(&b, acts...)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// writeFacts renders what the pipeline knows about an item above its events.
//
// This is the answer to "where do the facts live" rather than a second link in
// the notes breadcrumb: the identifiers an owner actually reads (GTIN, FCC ID,
// unit serial) are written onto the Homebox item as real fields, where they
// are visible and searchable; everything else is pipeline internals, and
// belongs next to the events that produced it. One link, one destination.
func (s *Server) writeFacts(ctx context.Context, b *strings.Builder, entityID string) {
	facts, err := s.st.Facts(ctx, entityID, "")
	if err != nil || len(facts) == 0 {
		return
	}
	label := map[string]string{
		store.FactGTIN: "GTIN", store.FactFCCID: "FCC ID",
		store.FactUnitSerial: "unit serial", store.FactProductType: "product type",
		store.FactFieldConf: "read confidence", store.FactLeadURL: "product page",
		store.FactSupportURL: "support",
	}
	b.WriteString(`<div class="logbox facts"><table class="logtbl"><tbody>`)
	for _, f := range facts {
		name := label[f.Kind]
		if name == "" {
			name = f.Kind
		}
		val := html.EscapeString(f.Value)
		if strings.HasPrefix(f.Value, "http") {
			val = fmt.Sprintf(`<a href="%s">%s</a>`,
				html.EscapeString(f.Value), html.EscapeString(shortURL(f.Value)))
		}
		fmt.Fprintf(b, `<tr><td class="lk">%s</td><td class="ld">%s</td><td class="lt">%s</td></tr>`,
			html.EscapeString(name), val, html.EscapeString(f.Source))
	}
	b.WriteString(`</tbody></table></div>`)
}

// handleEvents is the JSON feed behind the post-create near-live log panel:
// GET /api/events?entity=ID -> newest-first compact event rows.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Intake.LiveLogEnabled() {
		http.NotFound(w, r)
		return
	}
	id := r.URL.Query().Get("entity")
	if id == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("missing entity"))
		return
	}
	events, err := s.st.Events(r.Context(), id, 40)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	type row struct {
		Ts     string `json:"ts"`
		Actor  string `json:"actor"`
		Kind   string `json:"kind"`
		Class  string `json:"class"`
		URL    string `json:"url"`
		Detail string `json:"detail"`
	}
	out := make([]row, 0, len(events))
	for _, e := range events {
		out = append(out, row{
			Ts: e.Ts.Local().Format("15:04:05"), Actor: e.Actor, Kind: e.Kind,
			Class: e.Class, URL: e.URL, Detail: e.Detail,
		})
	}
	writeJSON(w, http.StatusOK, out)
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
