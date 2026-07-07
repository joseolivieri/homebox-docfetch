// Package config loads and validates the homebox-docfetch property file.
//
// The file is YAML. Any value of the form ${VAR} is replaced with the
// environment variable VAR before parsing, which is how secrets (the Homebox
// token, the OpenRouter key) are injected without ever living in the file.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the full property-file schema (see docs/spec.md §5). Sections that
// are not yet consumed (portal, vision) are parsed so the example file is valid
// and so Phase 2 needs no loader changes.
type Config struct {
	Homebox    Homebox    `yaml:"homebox"`
	Schedule   Schedule   `yaml:"schedule"`
	Discovery  Discovery  `yaml:"discovery"`
	LLM        LLM        `yaml:"llm"`
	Confidence Confidence `yaml:"confidence"`
	Attach     Attach     `yaml:"attach"`
	Intake     Intake     `yaml:"intake"`
	Enrich     Enrich     `yaml:"enrich"`
	Reconcile  Reconcile  `yaml:"reconcile"`
	Notify     Notify     `yaml:"notify"`
	Portal     Portal     `yaml:"portal"`
	Vision     Vision     `yaml:"vision"`
	StateDB    string     `yaml:"state_db"`
}

// Enrich configures metadata auto-completion (Phase 1.5). Fill-only by design.
type Enrich struct {
	Enabled            bool     `yaml:"enabled"`
	FillOnly           bool     `yaml:"fill_only"`
	AutoWriteThreshold float64  `yaml:"auto_write_threshold"`
	MinAgreeingSources int      `yaml:"min_agreeing_sources"`
	BackCheck          bool     `yaml:"back_check"`
	Fields             []string `yaml:"fields"`
	ProvenanceNote     bool     `yaml:"provenance_note"`
}

type Homebox struct {
	URL      string `yaml:"url"`
	Token    string `yaml:"token"`
	PageSize int    `yaml:"page_size"`
}

type Schedule struct {
	ScanNew       string        `yaml:"scan_new"`
	Followup      string        `yaml:"followup"`
	FollowupAfter time.Duration `yaml:"followup_after"`
}

type Discovery struct {
	SearxngURL      string        `yaml:"searxng_url"`
	Queries         []string      `yaml:"queries"`
	MaxCandidates   int           `yaml:"max_candidates"`
	MinPDFBytes     int64         `yaml:"min_pdf_bytes"`
	MaxPDFBytes     int64         `yaml:"max_pdf_bytes"`
	RateLimitPerMin int           `yaml:"rate_limit_per_min"`
	BackoffBase     time.Duration `yaml:"backoff_base"`
}

type LLM struct {
	BaseURL         string `yaml:"base_url"`
	APIKey          string `yaml:"api_key"`
	RerankModel     string `yaml:"rerank_model"`
	VisionModel     string `yaml:"vision_model"`
	MaxSnippetChars int    `yaml:"max_snippet_chars"`
}

type Confidence struct {
	AutoAttachThreshold float64 `yaml:"auto_attach_threshold"`
	RequireModelMatch   bool    `yaml:"require_model_match"`
}

type Attach struct {
	DocType            string `yaml:"doc_type"`
	SkipIfManualExists bool   `yaml:"skip_if_manual_exists"`
}

type Intake struct {
	UnverifiedTag string `yaml:"unverified_tag"`
	ProvenanceTag string `yaml:"provenance_tag"`
}

type Reconcile struct {
	DigestSchedule string `yaml:"digest_schedule"`
}

type Notify struct {
	NtfyURL   string `yaml:"ntfy_url"`
	NtfyTopic string `yaml:"ntfy_topic"`
	NtfyToken string `yaml:"ntfy_token"` // optional bearer for restricted publish
}

type Portal struct {
	Listen             string   `yaml:"listen"`
	LocationEntityType string   `yaml:"location_entity_type"`
	DefaultLocation    string   `yaml:"default_location"`
	IntakePhotos       []string `yaml:"intake_photos"`
	WarrantyEstimate   bool     `yaml:"warranty_estimate"`
}

type Vision struct {
	// reserved for Phase 2; vision model id lives under llm.vision_model
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
	if c.Discovery.MaxCandidates == 0 {
		c.Discovery.MaxCandidates = 8
	}
	if c.LLM.MaxSnippetChars == 0 {
		c.LLM.MaxSnippetChars = 150
	}
	if c.LLM.BaseURL == "" {
		c.LLM.BaseURL = "https://openrouter.ai/api/v1"
	}
	if c.Attach.DocType == "" {
		c.Attach.DocType = "manual"
	}
	if c.StateDB == "" {
		c.StateDB = "/data/docfetch.db"
	}
}

func (c *Config) validate() error {
	var missing []string
	req := map[string]string{
		"homebox.url":           c.Homebox.URL,
		"homebox.token":         c.Homebox.Token,
		"intake.unverified_tag": c.Intake.UnverifiedTag,
		"intake.provenance_tag": c.Intake.ProvenanceTag,
	}
	for k, v := range req {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: missing required values: %v", missing)
	}
	if c.Confidence.AutoAttachThreshold < 0 || c.Confidence.AutoAttachThreshold > 1 {
		return fmt.Errorf("config: confidence.auto_attach_threshold must be within [0,1], got %v", c.Confidence.AutoAttachThreshold)
	}
	return nil
}
