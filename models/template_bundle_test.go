package models

import (
	"testing"

	null "gopkg.in/volatiletech/null.v6"
)

const campaignBundleBody = `<html>{{ template "content" . }}</html>`

func validBundle() TemplateBundle {
	return TemplateBundle{Templates: []BundleTemplate{
		{
			Key:       "camp-default",
			Name:      "Default campaign",
			Type:      TemplateTypeCampaign,
			Body:      campaignBundleBody,
			IsDefault: true,
		},
		{
			Key:     "tx-welcome",
			Name:    "Welcome email",
			Type:    TemplateTypeTx,
			Subject: "Hi {{ .Subscriber.Name }}",
			Body:    "<p>Welcome</p>",
		},
	}}
}

func TestBundleValidateOK(t *testing.T) {
	if errs := validBundle().Validate(); len(errs) != 0 {
		t.Fatalf("expected no validation errors, got %v", errs)
	}
}

func TestBundleValidateDuplicateKeys(t *testing.T) {
	b := validBundle()
	b.Templates = append(b.Templates, BundleTemplate{
		Key:     "tx-welcome",
		Name:    "Duplicate",
		Type:    TemplateTypeTx,
		Subject: "dup subject",
		Body:    "<p>dup</p>",
	})

	errs := b.Validate()
	if len(errs) == 0 {
		t.Fatal("expected duplicate key error, got none")
	}

	found := false
	for _, e := range errs {
		if e.Key == "tx-welcome" {
			found = true
			if e.Reason != "duplicate template key in bundle" {
				t.Fatalf("unexpected reason: %q", e.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("expected error to name key %q, got %v", "tx-welcome", errs)
	}
}

func TestBundleValidateTwoDefaults(t *testing.T) {
	b := validBundle()
	b.Templates = append(b.Templates, BundleTemplate{
		Key:       "camp-other",
		Name:      "Other campaign",
		Type:      TemplateTypeCampaign,
		Body:      campaignBundleBody,
		IsDefault: true,
	})

	errs := b.Validate()
	named := map[string]bool{}
	for _, e := range errs {
		if e.Reason == "bundle declares more than one default template" {
			named[e.Key] = true
		}
	}
	if !named["camp-default"] || !named["camp-other"] {
		t.Fatalf("expected both default keys to be named, got %v", errs)
	}
}

func TestBundleValidateDefaultOnTxRejected(t *testing.T) {
	b := TemplateBundle{Templates: []BundleTemplate{
		{Key: "tx-bad", Name: "Bad", Type: TemplateTypeTx, Subject: "s", Body: "b", IsDefault: true},
	}}
	errs := b.Validate()
	found := false
	for _, e := range errs {
		if e.Key == "tx-bad" && e.Reason == "only campaign templates can be marked default" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected default-on-tx error, got %v", errs)
	}
}

func TestBundleValidateFieldRules(t *testing.T) {
	b := TemplateBundle{Templates: []BundleTemplate{
		// tx without subject.
		{Key: "tx-no-subject", Name: "No subject", Type: TemplateTypeTx, Body: "b"},
		// campaign without content placeholder.
		{Key: "camp-no-tag", Name: "No tag", Type: TemplateTypeCampaign, Body: "<p>no placeholder</p>"},
		// invalid type, empty body and name.
		{Key: "weird", Name: "", Type: "nope", Body: ""},
		// empty key.
		{Key: "", Name: "No key", Type: TemplateTypeTx, Subject: "s", Body: "b"},
	}}

	errs := b.Validate()
	byKey := map[string][]string{}
	for _, e := range errs {
		byKey[e.Key] = append(byKey[e.Key], e.Reason)
	}

	for _, key := range []string{"tx-no-subject", "camp-no-tag", "weird", ""} {
		if len(byKey[key]) == 0 {
			t.Fatalf("expected errors for key %q, got %v", key, errs)
		}
	}
}

func TestBundleDefaultKey(t *testing.T) {
	b := validBundle()
	k, ok := b.DefaultKey()
	if !ok || k != "camp-default" {
		t.Fatalf("unexpected default: %q %v", k, ok)
	}

	b.Templates[0].IsDefault = false
	if _, ok := b.DefaultKey(); ok {
		t.Fatal("expected no default")
	}
}

func TestBundleErrorError(t *testing.T) {
	if got := (BundleError{Key: "k", Reason: "r"}).Error(); got != `template key "k": r` {
		t.Fatalf("unexpected error string: %q", got)
	}
}

func TestNullStringEqualityHelper(t *testing.T) {
	// null.String is used for body_source; ensure equality semantics used by
	// the sync diff distinguish NULL from an empty string.
	a := null.String{Valid: false}
	b := null.String{String: "", Valid: true}
	if a == b {
		t.Fatal("NULL and empty-string body_source must differ")
	}
}
