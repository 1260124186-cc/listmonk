package manager

import (
	"strings"
	"testing"

	"github.com/knadh/listmonk/models"
)

func TestValidateBundleRejectsBrokenTemplateWithKey(t *testing.T) {
	m := newTestManager(t)

	badTx := models.TemplateBundle{Templates: []models.BundleTemplate{
		{Key: "tx-broken", Name: "Broken tx", Type: models.TemplateTypeTx, Subject: "ok", Body: `{{ .Subscriber.Name `},
	}}
	err := m.ValidateBundle(badTx)
	if err == nil {
		t.Fatal("expected render validation to fail on a broken tx template")
	}
	if !strings.Contains(err.Error(), "tx-broken") {
		t.Fatalf("error must name the offending key: %v", err)
	}

	badCampaign := models.TemplateBundle{Templates: []models.BundleTemplate{
		{Key: "camp-broken", Name: "Broken campaign", Type: models.TemplateTypeCampaign, Body: `{{ template "content" . }}{{ if .Foo }}`},
	}}
	err = m.ValidateBundle(badCampaign)
	if err == nil || !strings.Contains(err.Error(), "camp-broken") {
		t.Fatalf("expected campaign render error named with its key, got: %v", err)
	}

	// A broken subject must be reported as well. The expression keeps both
	// delimiters so it is recognised as a template and fails parsing.
	badSubject := models.TemplateBundle{Templates: []models.BundleTemplate{
		{Key: "tx-subject", Name: "Bad subject", Type: models.TemplateTypeTx, Subject: `{{ if }}`, Body: "ok"},
	}}
	if err := m.ValidateBundle(badSubject); err == nil ||
		!strings.Contains(err.Error(), "tx-subject") {
		t.Fatalf("expected subject render error named with its key, got: %v", err)
	}
}

func TestValidateBundleAcceptsValidBundle(t *testing.T) {
	m := newTestManager(t)

	good := models.TemplateBundle{Templates: []models.BundleTemplate{
		{
			Key:       "camp-default",
			Name:      "Default campaign",
			Type:      models.TemplateTypeCampaign,
			Body:      `<!doctype html><html><body>{{ template "content" . }}</body></html>`,
			IsDefault: true,
		},
		{
			Key:     "tx-welcome",
			Name:    "Welcome",
			Type:    models.TemplateTypeTx,
			Subject: "Hi {{ .Subscriber.Name }}",
			Body:    "<p>Welcome {{ .Subscriber.Name }}</p>",
		},
	}}
	if err := m.ValidateBundle(good); err != nil {
		t.Fatalf("valid bundle rejected: %v", err)
	}
}
