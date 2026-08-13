package portal

import (
	"regexp"
	"strings"
)

// Payload classification for decoded 2D/1D codes (plan §6.4). The point is
// ROUTING, not just filtering: some non-URL payloads carry identity and must
// be parsed, and some URL payloads must never be chased.
//
// Deliberately not a per-vendor integration — Amazon Transparency's GTIN form,
// GS1 Digital Link and GS1 element strings share one Application Identifier
// structure, so one parser covers all three (and Sunrise 2027's retail
// migration to 2D Digital Link codes) with no vendor-specific code.
type PayloadKind string

const (
	PayloadDigitalLink PayloadKind = "gs1-digital-link" // URL carrying GS1 AIs
	PayloadElement     PayloadKind = "gs1-element"      // (01)…(21)… element string
	PayloadAuthToken   PayloadKind = "auth-token"       // opaque unit token (AZ/ZA…)
	PayloadMatter      PayloadKind = "matter"           // MT:… smart-home setup code
	PayloadSupportURL  PayloadKind = "support-url"      // chase it
	PayloadPlatform    PayloadKind = "platform"         // provenance only, never chased
	PayloadNonWeb      PayloadKind = "non-web"          // WIFI:, mailto:, tel: — drop
	PayloadUnknown     PayloadKind = "unknown"          // safe default: record, don't chase
)

// Payload is a classified code with any identifiers parsed out of it.
type Payload struct {
	Kind   PayloadKind
	Raw    string
	URL    string // set when the payload is fetchable (support-url, digital-link)
	GTIN   string // AI 01
	Serial string // AI 21 — unit-unique; NEVER send to a third party
}

// Chaseable reports whether curation may fetch this payload. Auth tokens are
// unit-unique tracking identifiers: chasing one would leak it to a search
// engine or vendor for no benefit.
func (p Payload) Chaseable() bool {
	return p.URL != "" && (p.Kind == PayloadSupportURL || p.Kind == PayloadDigitalLink)
}

var (
	// /01/<gtin>/21/<serial> style Digital Link path segments.
	dlGTIN   = regexp.MustCompile(`/01/(\d{8,14})`)
	dlSerial = regexp.MustCompile(`/21/([^/?#]{1,40})`)
	// (01)<gtin>(21)<serial> element strings, parenthesised or bare.
	esGTIN   = regexp.MustCompile(`\(01\)(\d{8,14})`)
	esSerial = regexp.MustCompile(`\(21\)([^()]{1,40})`)
	// Amazon Transparency alphanumeric form: AZ/ZA + 26 alphanumerics.
	azToken = regexp.MustCompile(`^(?i)(AZ|ZA)[0-9A-Z]{26}$`)
	// Bare GS1 element string without parens, e.g. 01095201234567882112345.
	bareAI01 = regexp.MustCompile(`^01(\d{14})`)
)

// ClassifyPayload types one decoded code payload and extracts any identifiers.
func ClassifyPayload(raw string) Payload {
	s := strings.TrimSpace(raw)
	p := Payload{Raw: s, Kind: PayloadUnknown}
	if s == "" {
		return p
	}
	low := strings.ToLower(s)

	switch {
	case strings.HasPrefix(low, "mt:"):
		p.Kind = PayloadMatter
		return p
	case azToken.MatchString(s):
		// Unit auth token: real provenance, zero identity, never chased.
		p.Kind = PayloadAuthToken
		return p
	case strings.HasPrefix(low, "http://"), strings.HasPrefix(low, "https://"):
		p.URL = s
	default:
		// Non-URL: either a GS1 element string or something we don't handle.
		if m := esGTIN.FindStringSubmatch(s); m != nil {
			p.Kind, p.GTIN = PayloadElement, m[1]
			if sm := esSerial.FindStringSubmatch(s); sm != nil {
				p.Serial = sm[1]
			}
			return p
		}
		if m := bareAI01.FindStringSubmatch(s); m != nil {
			p.Kind, p.GTIN = PayloadElement, m[1]
			return p
		}
		p.Kind = PayloadNonWeb
		return p
	}

	// From here the payload is an http(s) URL.
	if m := dlGTIN.FindStringSubmatch(s); m != nil {
		// GS1 Digital Link: identity AND a resolvable URL by design.
		p.Kind, p.GTIN = PayloadDigitalLink, m[1]
		if sm := dlSerial.FindStringSubmatch(s); sm != nil {
			p.Serial = sm[1]
		}
		return p
	}
	if isPlatformURL(low) {
		p.Kind = PayloadPlatform
		return p
	}
	p.Kind = PayloadSupportURL
	return p
}

// isPlatformURL mirrors the discovery-side platform check. Duplicated rather
// than imported: the intake stage must not depend on discovery/egress code.
func isPlatformURL(low string) bool {
	for _, h := range []string{
		"youtube.", "youtu.be", "vimeo.", "tiktok.",
		"facebook.", "instagram.", "twitter.", "x.com/", "linktr.ee",
	} {
		if strings.Contains(low, h) {
			return true
		}
	}
	return false
}
