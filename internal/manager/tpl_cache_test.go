package manager

import (
	"log"
	"os"
	"testing"

	"github.com/knadh/listmonk/internal/i18n"
	"github.com/knadh/listmonk/models"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()

	lang, err := os.ReadFile("../../i18n/en.json")
	if err != nil {
		t.Fatalf("reading en.json: %v", err)
	}
	i, err := i18n.New(lang)
	if err != nil {
		t.Fatalf("i18n init: %v", err)
	}

	// A nil store is fine: caching and rendering a plain tx template never
	// touch the store.
	return New(Config{}, nil, i, log.New(os.Stdout, "", log.LstdFlags))
}

func renderCachedTx(t *testing.T, m *Manager, id int) string {
	t.Helper()

	tpl, err := m.GetTpl(id)
	if err != nil {
		t.Fatalf("GetTpl: %v", err)
	}
	msg := models.TxMessage{}
	if err := msg.Render(models.Subscriber{Name: "Jane"}, tpl, m.GenericTemplateFuncs()); err != nil {
		t.Fatalf("render: %v", err)
	}
	return string(msg.Body)
}

// TestCacheTplReplacesVersion proves that re-caching a transactional template
// immediately swaps the version used by the next send, and that deletion
// evicts it. This is the guarantee relied on by template bundle sync.
func TestCacheTplReplacesVersion(t *testing.T) {
	m := newTestManager(t)
	const id = 42

	v1 := &models.Template{Type: models.TemplateTypeTx, Subject: "s", Body: "version one for {{ .Subscriber.Name }}"}
	if err := v1.Compile(m.GenericTemplateFuncs()); err != nil {
		t.Fatalf("compile v1: %v", err)
	}
	m.CacheTpl(id, v1)
	if got := renderCachedTx(t, m, id); got != "version one for Jane" {
		t.Fatalf("unexpected v1 render: %q", got)
	}

	// Simulate a bundle sync updating the same template ID.
	v2 := &models.Template{Type: models.TemplateTypeTx, Subject: "s", Body: "version two for {{ .Subscriber.Name }}"}
	if err := v2.Compile(m.GenericTemplateFuncs()); err != nil {
		t.Fatalf("compile v2: %v", err)
	}
	m.CacheTpl(id, v2)
	if got := renderCachedTx(t, m, id); got != "version two for Jane" {
		t.Fatalf("next send did not use the new template version: %q", got)
	}

	m.DeleteTpl(id)
	if _, err := m.GetTpl(id); err == nil {
		t.Fatal("template should be evicted after DeleteTpl")
	}
}
