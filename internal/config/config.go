// Package config loads and validates the homebox-docfetch property file.
//
// The file is YAML. Any value of the form ${VAR} is replaced with the
// environment variable VAR before parsing, which is how secrets (the Homebox
// token, the OpenRouter key) are injected without ever living in the file.
//
// The schema mirrors the two pipeline stages (docs/how-it-works.md):
//
//   - intake:   the phone portal. Vision-model calls ONLY — no web searching,
//     so the LLM can eventually move local/offline.
//   - curation: the recurring scanner. ALL web egress lives here — metadata
//     enrichment, doc discovery, official photos, warranty lookups, tagging.
//
// Everything else (homebox, llm, tags, notify, notes, state_db) is shared.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the full property-file schema (see docs/spec.md §5).
type Config struct {
	Homebox  Homebox  `yaml:"homebox"`
	LLM      LLM      `yaml:"llm"`
	Tags     Tags     `yaml:"tags"`
	Notify   Notify   `yaml:"notify"`
	Notes    Notes    `yaml:"notes"`
	Intake   Intake   `yaml:"intake"`
	Curation Curation `yaml:"curation"`
	StateDB  string   `yaml:"state_db"`
}

type Homebox struct {
	URL      string `yaml:"url"`
	Token    string `yaml:"token"`
	PageSize int    `yaml:"page_size"`
}

type LLM struct {
	BaseURL         string `yaml:"base_url"`
	APIKey          string `yaml:"api_key"`
	RerankModel     string `yaml:"rerank_model"`
	VisionModel     string `yaml:"vision_model"`
	MaxSnippetChars int    `yaml:"max_snippet_chars"`
}

// Tags are the triage/provenance markers (this fork's "labels"), shared by
// both stages.
type Tags struct {
	Unverified string `yaml:"unverified"`
	Provenance string `yaml:"provenance"`
}

type Notify struct {
	NtfyURL   string `yaml:"ntfy_url"`
	NtfyTopic string `yaml:"ntfy_topic"`
	NtfyToken string `yaml:"ntfy_token"` // optional bearer for restricted publish
}

// Notes tunes the machine-written log block in each entity's notes field.
type Notes struct {
	// AuditLog opt-in: log a terse line for EVERY derived write (intake photos,
	// official photo, warranty, metadata) with confidence scores. Off = only
	// doc attach/link events are logged.
	AuditLog bool `yaml:"audit_log"`
}

// Intake — stage 1, the phone portal. Vision extraction + item creation only.
type Intake struct {
	Listen             string   `yaml:"listen"`
	PublicURL          string   `yaml:"public_url"` // e.g. https://docfetch.ingress-1...; target of ntfy action buttons
	LocationEntityType string   `yaml:"location_entity_type"`
	Photos             []string `yaml:"photos"` // intake photo slots (documentation; UI is fixed)
}

// Curation — stage 2, the recurring scanner. All web searching happens here.
type Curation struct {
	Schedule  Schedule  `yaml:"schedule"`
	Discovery Discovery `yaml:"discovery"`
	Docs      Docs      `yaml:"docs"`
	Enrich    Enrich    `yaml:"enrich"`
	Photo     Photo     `yaml:"photo"`
	Warranty  Warranty  `yaml:"warranty"`
	Reconcile Reconcile `yaml:"reconcile"`
}

type Schedule struct {
	ScanNew       string        `yaml:"scan_new"`
	Followup      string        `yaml:"followup"`
	FollowupAfter time.Duration `yaml:"followup_after"`
	ChangePoll    time.Duration `yaml:"change_poll"` // 0 disables; probe-for-changes interval
}

type Discovery struct {
	SearxngURL      string        `yaml:"searxng_url"`
	Language        string        `yaml:"language"` // SearXNG language code (e.g. "en", "en-US"); biases manual/warranty/photo sources
	Region          string        `yaml:"region"`   // ISO country code (e.g. "us"); other-market URLs deprioritized in docs/photos/warranty. "" = off
	Pipeline        []string      `yaml:"pipeline"` // source priority: brand-site, web-pdf, web-html
	Queries         []string      `yaml:"queries"`
	MaxCandidates   int           `yaml:"max_candidates"`
	MinPDFBytes     int64         `yaml:"min_pdf_bytes"`
	MaxPDFBytes     int64         `yaml:"max_pdf_bytes"`
	RateLimitPerMin int           `yaml:"rate_limit_per_min"`
	BackoffBase     time.Duration `yaml:"backoff_base"`
}

// Docs configures document fetching/attaching. Enabled is a pointer so that an
// absent key means true — docs are the core provider.
type Docs struct {
	Enabled             *bool      `yaml:"enabled"`
	SkipIfExists        bool       `yaml:"skip_if_exists"`
	AutoAttachThreshold float64    `yaml:"auto_attach_threshold"`
	RequireModelMatch   bool       `yaml:"require_model_match"`
	Classes             []DocClass `yaml:"classes"`
}

// DocClass is one fetchable document kind (manual, parts, quickstart…). Each
// class selects its OWN best candidate and attaches independently, so an
// appliance gets both a manual and a parts list instead of one competing for
// a single slot. Classification is by keyword match on the candidate's
// url/title/snippet; category gating limits a class to relevant item types.
type DocClass struct {
	Name       string   `yaml:"name"`       // ledger doc_class + notes verb ("manual", "parts")
	Field      string   `yaml:"field"`      // custom-field label ("Manual" -> "Manual"/"Manual (web)")
	AttachAs   string   `yaml:"attach_as"`  // Homebox attachment type (manual|attachment|warranty)
	Keywords   []string `yaml:"keywords"`   // classify + page-follow harvest keep-set + query hints
	Queries    []string `yaml:"queries"`    // {subject} search templates for the web stages
	Categories []string `yaml:"categories"` // only fetch for items whose tags match one of these; empty = all
	Enabled    bool     `yaml:"enabled"`
}

// DocsEnabled resolves the docs provider toggle (default true).
func (c *Config) DocsEnabled() bool {
	return c.Curation.Docs.Enabled == nil || *c.Curation.Docs.Enabled
}

// defaultDocClasses is used when the property file defines none. manual keeps
// today's behavior; parts is category-gated to appliances/tools so earbuds
// never fetch a parts list; quickstart/datasheet are defined but off.
func defaultDocClasses() []DocClass {
	return []DocClass{
		{
			Name: "manual", Field: "Manual", AttachAs: "manual", Enabled: true,
			Keywords: []string{"manual", "guide", "owner", "use and care", "use & care", "instruction", "user", "handbook", "knowledge-download", "_um", "_ug"},
			Queries:  []string{"{subject} user manual filetype:pdf", "{subject} owner's manual pdf"},
		},
		{
			Name: "parts", Field: "Parts", AttachAs: "attachment", Enabled: true,
			Keywords:   []string{"parts", "parts list", "parts diagram", "exploded", "spare", "schematic", "service manual", "repair"},
			Queries:    []string{"{subject} parts list filetype:pdf", "{subject} parts diagram pdf"},
			Categories: []string{"appliance", "dishwasher", "washer", "dryer", "refrigerator", "fridge", "freezer", "oven", "range", "stove", "microwave", "hvac", "furnace", "water heater", "tool", "mower", "vacuum", "grill"},
		},
		{
			Name: "quickstart", Field: "Quick start", AttachAs: "attachment", Enabled: false,
			Keywords: []string{"quick start", "quickstart", "qsg", "quick setup", "getting started", "setup guide"},
			Queries:  []string{"{subject} quick start guide filetype:pdf"},
		},
		{
			Name: "datasheet", Field: "Datasheet", AttachAs: "attachment", Enabled: false,
			Keywords: []string{"datasheet", "data sheet", "specification", "spec sheet", "technical data"},
			Queries:  []string{"{subject} datasheet filetype:pdf"},
		},
	}
}

// Enrich configures metadata auto-completion. Fill-only by design.
type Enrich struct {
	Enabled            bool     `yaml:"enabled"`
	FillOnly           bool     `yaml:"fill_only"`
	AutoWriteThreshold float64  `yaml:"auto_write_threshold"`
	MinAgreeingSources int      `yaml:"min_agreeing_sources"`
	BackCheck          bool     `yaml:"back_check"`
	Fields             []string `yaml:"fields"`
	ProvenanceNote     bool     `yaml:"provenance_note"`
}

// Photo configures official-product-photo fetching (curation stage).
type Photo struct {
	Enabled       bool    `yaml:"enabled"`
	MinConfidence float64 `yaml:"min_confidence"` // below the bar, NO photo attaches; 0 -> 0.7
}

// Warranty configures warranty estimation (curation stage).
type Warranty struct {
	Enabled bool `yaml:"enabled"`
}

type Reconcile struct {
	DigestSchedule string `yaml:"digest_schedule"`
}

var envRef = regexp.MustCompile(`\$\{(\w+)\}`)

// Load reads, env-interpolates, parses, and validates the config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	expanded := envRef.ReplaceAllStringFunc(string(raw), func(m string) string {
		return os.Getenv(envRef.FindStringSubmatch(m)[1])
	})

	var c Config
	if err := yaml.Unmarshal([]byte(expanded), &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.defaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) defaults() {
	if c.Homebox.PageSize == 0 {
		c.Homebox.PageSize = 100
	}
	if c.Curation.Discovery.MaxCandidates == 0 {
		c.Curation.Discovery.MaxCandidates = 8
	}
	if c.LLM.MaxSnippetChars == 0 {
		c.LLM.MaxSnippetChars = 150
	}
	if c.LLM.BaseURL == "" {
		c.LLM.BaseURL = "https://openrouter.ai/api/v1"
	}
	if len(c.Curation.Docs.Classes) == 0 {
		c.Curation.Docs.Classes = defaultDocClasses()
	}
	for i := range c.Curation.Docs.Classes {
		dc := &c.Curation.Docs.Classes[i]
		if dc.Field == "" {
			dc.Field = strings.Title(dc.Name) //nolint:staticcheck // ASCII class names
		}
		if dc.AttachAs == "" {
			if dc.Name == "manual" {
				dc.AttachAs = "manual"
			} else {
				dc.AttachAs = "attachment"
			}
		}
	}
	if c.Curation.Discovery.Language == "" {
		c.Curation.Discovery.Language = "en"
	}
	if c.Curation.Discovery.Region == "" && strings.HasPrefix(c.Curation.Discovery.Language, "en") {
		// English defaults to the US market; non-English deployments must opt in.
		c.Curation.Discovery.Region = "us"
	}
	if c.Curation.Photo.MinConfidence == 0 {
		c.Curation.Photo.MinConfidence = 0.7
	}
	if c.Intake.Listen == "" {
		c.Intake.Listen = ":8099"
	}
	if c.StateDB == "" {
		c.StateDB = "/data/docfetch.db"
	}
}

func (c *Config) validate() error {
	var missing []string
	req := map[string]string{
		"homebox.url":     c.Homebox.URL,
		"homebox.token":   c.Homebox.Token,
		"tags.unverified": c.Tags.Unverified,
		"tags.provenance": c.Tags.Provenance,
	}
	for k, v := range req {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: missing required values: %v", missing)
	}
	if t := c.Curation.Docs.AutoAttachThreshold; t < 0 || t > 1 {
		return fmt.Errorf("config: curation.docs.auto_attach_threshold must be within [0,1], got %v", t)
	}
	return nil
}
