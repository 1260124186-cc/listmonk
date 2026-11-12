package models

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	null "gopkg.in/volatiletech/null.v6"
)

// BundleContentTagRegexp matches the campaign content placeholder that must
// appear exactly once in a campaign template body. It mirrors the check used
// by the admin template handlers.
var BundleContentTagRegexp = regexp.MustCompile(`{{(\s+)?template\s+?"content"(\s+)?\.(\s+)?}}`)

// BundleTemplate is a single template entry carried by a template bundle.
// Key is the stable identity used to reconcile bundle templates against the
// templates already stored in the DB.
type BundleTemplate struct {
	Key        string      `json:"key"`
	Name       string      `json:"name"`
	Type       string      `json:"type"`
	Subject    string      `json:"subject"`
	Body       string      `json:"body"`
	BodySource null.String `json:"body_source,omitempty"`
	IsDefault  bool        `json:"is_default"`
}

// TemplateBundle is the full design-team template package that a sync
// reconciles against the DB.
type TemplateBundle struct {
	Templates []BundleTemplate `json:"templates"`
}

// BundleError describes a validation failure on a specific bundle entry.
// A sync is rejected wholesale if any entry produces an error.
type BundleError struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

func (e BundleError) Error() string {
	if e.Key == "" {
		return e.Reason
	}
	return fmt.Sprintf("template key %q: %s", e.Key, e.Reason)
}

// Validate performs all checks that can be done without touching the DB or
// rendering the templates: structural field rules, duplicate keys and the
// number of default templates. It returns every problem found so the caller
// can report all affected template keys at once.
func (b TemplateBundle) Validate() []BundleError {
	errs := make([]BundleError, 0)

	seenKeys := make(map[string]int, len(b.Templates))
	defaults := make([]string, 0)
	for _, t := range b.Templates {
		key := strings.TrimSpace(t.Key)

		if key == "" {
			errs = append(errs, BundleError{Key: t.Key, Reason: "empty template key"})
		} else {
			seenKeys[key]++
		}

		if strings.TrimSpace(t.Name) == "" {
			errs = append(errs, BundleError{Key: key, Reason: "empty name"})
		}

		switch t.Type {
		case TemplateTypeCampaign, TemplateTypeCampaignVisual, TemplateTypeTx:
		default:
			errs = append(errs, BundleError{Key: key, Reason: fmt.Sprintf("invalid type %q", t.Type)})
		}

		if strings.TrimSpace(t.Body) == "" {
			errs = append(errs, BundleError{Key: key, Reason: "empty body"})
		}

		if t.Type == TemplateTypeCampaign && !BundleContentTagRegexp.MatchString(t.Body) {
			errs = append(errs, BundleError{Key: key, Reason: `campaign body must contain the {{ template "content" . }} placeholder`})
		}

		if t.Type == TemplateTypeTx && strings.TrimSpace(t.Subject) == "" {
			errs = append(errs, BundleError{Key: key, Reason: "transactional template requires a subject"})
		}

		if t.IsDefault {
			defaults = append(defaults, key)
			if t.Type != TemplateTypeCampaign {
				errs = append(errs, BundleError{Key: key, Reason: "only campaign templates can be marked default"})
			}
		}
	}

	// Duplicate keys. Report each duplicated key once, deterministically.
	dupKeys := make([]string, 0)
	for key, n := range seenKeys {
		if n > 1 {
			dupKeys = append(dupKeys, key)
		}
	}
	sort.Strings(dupKeys)
	for _, key := range dupKeys {
		errs = append(errs, BundleError{Key: key, Reason: "duplicate template key in bundle"})
	}

	// More than one default template. The partial unique index only allows a
	// single default row, so a bundle carrying several cannot be applied.
	if len(defaults) > 1 {
		sort.Strings(defaults)
		for _, key := range defaults {
			errs = append(errs, BundleError{Key: key, Reason: "bundle declares more than one default template"})
		}
	}

	return errs
}

// DefaultKey returns the key of the bundle's default campaign template and
// whether one was declared.
func (b TemplateBundle) DefaultKey() (string, bool) {
	for _, t := range b.Templates {
		if t.IsDefault {
			return strings.TrimSpace(t.Key), true
		}
	}
	return "", false
}

// TemplateSyncResult is a single template affected by a sync run.
type TemplateSyncResult struct {
	Key  string `json:"key"`
	ID   int    `json:"id"`
	Type string `json:"type"`
}

// CampaignReassignment records that campaigns using a removed template were
// moved onto the bundle's default campaign template before deletion.
type CampaignReassignment struct {
	FromTemplateID int `json:"from_template_id"`
	ToTemplateID   int `json:"to_template_id"`
	Campaigns      int `json:"campaigns"`
}

// TemplateSyncReport is the reconciliation result returned by a sync.
type TemplateSyncReport struct {
	Created    []TemplateSyncResult   `json:"created"`
	Updated    []TemplateSyncResult   `json:"updated"`
	Deleted    []TemplateSyncResult   `json:"deleted"`
	Unchanged  []string               `json:"unchanged"`
	Reassigned []CampaignReassignment `json:"reassigned_campaigns"`
}

func NewTemplateSyncReport() TemplateSyncReport {
	return TemplateSyncReport{
		Created:    make([]TemplateSyncResult, 0),
		Updated:    make([]TemplateSyncResult, 0),
		Deleted:    make([]TemplateSyncResult, 0),
		Unchanged:  make([]string, 0),
		Reassigned: make([]CampaignReassignment, 0),
	}
}
