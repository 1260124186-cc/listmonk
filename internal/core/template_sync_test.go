package core

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/goyesql/v2"
	goyesqlx "github.com/knadh/goyesql/v2/sqlx"
	"github.com/knadh/listmonk/internal/i18n"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v5"
	null "gopkg.in/volatiletech/null.v6"
)

const testDSNDefault = "postgres://listmonk:listmonk@127.0.0.1:15545/listmonk_test?sslmode=disable"

// TestMain installs the fresh schema once; individual tests truncate all
// tables instead of replaying the whole DDL set.
func TestMain(m *testing.M) {
	db, err := sqlx.Connect("postgres", testDSN())
	if err != nil {
		// Let the per-test skip path report the missing DB.
		os.Exit(m.Run())
	}
	schema, err := os.ReadFile(filepath.Join("..", "..", "schema.sql"))
	if err != nil {
		log.Fatalf("reading schema.sql: %v", err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		log.Fatalf("loading schema: %v", err)
	}
	db.Close()
	os.Exit(m.Run())
}

// truncateAll wipes every table in the public schema while keeping it intact.
func truncateAll(t *testing.T, db *sqlx.DB) {
	t.Helper()
	if _, err := db.Exec(`
		DO $$
		DECLARE r record;
		BEGIN
			FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' LOOP
				EXECUTE 'TRUNCATE TABLE ' || quote_ident(r.tablename) || ' CASCADE';
			END LOOP;
		END $$;
	`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}
}

const campBody = `<!doctype html><html><body>{{ template "content" . }}</body></html>`

func testDSN() string {
	if dsn := os.Getenv("LISTMONK_TEST_DSN"); dsn != "" {
		return dsn
	}
	return testDSNDefault
}

// newTestCore resets the database to the fresh schema and returns a Core wired
// exactly like the application's core (same query files, prepared the same way).
func newTestCore(t *testing.T) *Core {
	t.Helper()

	db, err := sqlx.Connect("postgres", testDSN())
	if err != nil {
		t.Skipf("test database not available (%v); set LISTMONK_TEST_DSN to run", err)
	}
	t.Cleanup(func() { db.Close() })

	truncateAll(t, db)

	qMap := goyesql.Queries{}
	files, err := filepath.Glob(filepath.Join("..", "..", "queries", "*.sql"))
	if err != nil {
		t.Fatalf("glob queries: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		mp, err := goyesql.ParseBytes(b)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for name, q := range mp {
			// Interpolate the placeholders the app resolves at boot. These
			// statements are never executed by the sync tests.
			q.Query = strings.ReplaceAll(q.Query, "%order%", "id asc")
			q.Query = strings.ReplaceAll(q.Query, "FROM %s", "FROM campaign_views")
			q.Query = strings.ReplaceAll(q.Query, "(%s)", "(*)")
			qMap[name] = q
		}
	}

	// Fill stand-ins for queries the app synthesises at runtime; sync tests
	// never execute them, but ScanToStruct requires every tag to be present.
	typ := reflect.TypeOf(models.Queries{})
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("query")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if _, ok := qMap[name]; !ok {
			qMap[name] = &goyesql.Query{Query: "SELECT 1", Tags: map[string]string{"name": name}}
		}
	}

	var q models.Queries
	if err := goyesqlx.ScanToStruct(&q, qMap, db.Unsafe()); err != nil {
		t.Fatalf("preparing queries: %v", err)
	}

	lang, err := os.ReadFile(filepath.Join("..", "..", "i18n", "en.json"))
	if err != nil {
		t.Fatalf("reading en.json: %v", err)
	}
	i, err := i18n.New(lang)
	if err != nil {
		t.Fatalf("i18n init: %v", err)
	}

	return New(&Opt{I18n: i, DB: db.Unsafe(), Queries: &q, Log: log.New(os.Stdout, "", log.LstdFlags)}, nil)
}

func (c *Core) templateByKey(t *testing.T, key string) models.Template {
	t.Helper()
	var out models.Template
	if err := c.db.Get(&out, c.q.GetTemplateByKey, key); err != nil {
		t.Fatalf("fetching template %q: %v", key, err)
	}
	return out
}

func (c *Core) countTemplates(t *testing.T) int {
	t.Helper()
	var n int
	if err := c.db.Get(&n, "SELECT COUNT(*) FROM templates"); err != nil {
		t.Fatalf("counting templates: %v", err)
	}
	return n
}

func (c *Core) insertCampaign(t *testing.T, templateID int) int {
	t.Helper()
	var id int
	if err := c.db.Get(&id, `
		INSERT INTO campaigns (uuid, name, subject, from_email, body, messenger, template_id, content_type)
		VALUES (gen_random_uuid(), 'Test campaign', 'subject', 'from@example.com', 'body', 'email', $1, 'richtext')
		RETURNING id`, templateID); err != nil {
		t.Fatalf("inserting campaign: %v", err)
	}
	return id
}

func (c *Core) campaignTemplateID(t *testing.T, campaignID int) int {
	t.Helper()
	var id int
	if err := c.db.Get(&id, "SELECT template_id FROM campaigns WHERE id=$1", campaignID); err != nil {
		t.Fatalf("reading campaign template: %v", err)
	}
	return id
}

func baseBundle() models.TemplateBundle {
	return models.TemplateBundle{Templates: []models.BundleTemplate{
		{Key: "camp-default", Name: "Default campaign", Type: models.TemplateTypeCampaign, Body: campBody, IsDefault: true},
		{Key: "camp-extra", Name: "Extra campaign", Type: models.TemplateTypeCampaign, Body: campBody},
		{Key: "tx-welcome", Name: "Welcome", Type: models.TemplateTypeTx, Subject: "Hi {{ .Subscriber.Name }}", Body: "<p>welcome</p>"},
	}}
}

func wantHTTPStatus(t *testing.T, err error, status int) {
	t.Helper()
	he, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("expected echo.HTTPError, got %T: %v", err, err)
	}
	if he.Code != status {
		t.Fatalf("expected status %d, got %d (%s)", status, he.Code, he.Message)
	}
}

func TestSyncCreatesManagedTemplates(t *testing.T) {
	c := newTestCore(t)

	report, err := c.SyncTemplates(baseBundle())
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if len(report.Created) != 3 || len(report.Updated) != 0 || len(report.Deleted) != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}

	def := c.templateByKey(t, "camp-default")
	if !def.Managed || !def.IsDefault || def.Type != models.TemplateTypeCampaign || def.Subject != "" {
		t.Fatalf("default template not stored correctly: %+v", def)
	}

	tx := c.templateByKey(t, "tx-welcome")
	if !tx.Managed || tx.Type != models.TemplateTypeTx || tx.Subject != "Hi {{ .Subscriber.Name }}" {
		t.Fatalf("tx template not stored correctly: %+v", tx)
	}

	// Only one default row may exist.
	var n int
	c.db.Get(&n, "SELECT COUNT(*) FROM templates WHERE is_default=true")
	if n != 1 {
		t.Fatalf("expected exactly one default, got %d", n)
	}
}

func TestSyncIdempotent(t *testing.T) {
	c := newTestCore(t)

	if _, err := c.SyncTemplates(baseBundle()); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	var updatedAt string
	c.db.Get(&updatedAt, "SELECT MAX(updated_at)::text FROM templates")
	countBefore := c.countTemplates(t)

	report, err := c.SyncTemplates(baseBundle())
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(report.Created) != 0 || len(report.Updated) != 0 || len(report.Deleted) != 0 {
		t.Fatalf("re-run changed data: %+v", report)
	}
	if len(report.Unchanged) != 3 {
		t.Fatalf("expected 3 unchanged, got %v", report.Unchanged)
	}
	if c.countTemplates(t) != countBefore {
		t.Fatal("row count changed on identical re-run")
	}

	var updatedAtAfter string
	c.db.Get(&updatedAtAfter, "SELECT MAX(updated_at)::text FROM templates")
	if updatedAtAfter != updatedAt {
		t.Fatalf("updated_at changed on identical re-run: %s -> %s", updatedAt, updatedAtAfter)
	}
}

func TestSyncUpdatesInPlaceByKey(t *testing.T) {
	c := newTestCore(t)

	first, err := c.SyncTemplates(baseBundle())
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	var firstID int
	for _, r := range first.Created {
		if r.Key == "tx-welcome" {
			firstID = r.ID
		}
	}

	b := baseBundle()
	b.Templates[2].Body = "<p>welcome v2</p>"
	report, err := c.SyncTemplates(b)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(report.Updated) != 1 || report.Updated[0].Key != "tx-welcome" || report.Updated[0].ID != firstID {
		t.Fatalf("expected in-place update on the same row, got %+v", report)
	}
	if len(report.Created) != 0 {
		t.Fatalf("upgrade must not create a new template, got %+v", report.Created)
	}

	got := c.templateByKey(t, "tx-welcome")
	if got.ID != firstID || got.Body != "<p>welcome v2</p>" {
		t.Fatalf("template not updated in place: %+v", got)
	}
}

func TestSyncPreservesManualTemplates(t *testing.T) {
	c := newTestCore(t)

	manual, err := c.CreateTemplate("Hand made", models.TemplateTypeCampaign, "", []byte(campBody), null.String{})
	if err != nil {
		t.Fatalf("creating manual template: %v", err)
	}
	manualID := manual.ID

	// First sync then a sync that removes every managed entry.
	if _, err := c.SyncTemplates(baseBundle()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	empty := models.TemplateBundle{Templates: []models.BundleTemplate{
		{Key: "camp-default", Name: "Default campaign", Type: models.TemplateTypeCampaign, Body: campBody, IsDefault: true},
	}}
	report, err := c.SyncTemplates(empty)
	if err != nil {
		t.Fatalf("prune sync: %v", err)
	}
	if len(report.Deleted) != 2 {
		t.Fatalf("expected 2 managed deletions, got %+v", report.Deleted)
	}

	// The manual template must survive untouched and stay unmanaged.
	got, err := c.GetTemplate(manualID, false)
	if err != nil {
		t.Fatalf("manual template disappeared: %v", err)
	}
	if got.Managed || got.TemplateKey.Valid {
		t.Fatalf("manual template was marked managed: %+v", got)
	}
	if got.Name != "Hand made" {
		t.Fatalf("manual template content changed: %+v", got)
	}
}

func TestSyncRemovesAndReassignsCampaigns(t *testing.T) {
	c := newTestCore(t)

	if _, err := c.SyncTemplates(baseBundle()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	extra := c.templateByKey(t, "camp-extra")
	def := c.templateByKey(t, "camp-default")

	campID := c.insertCampaign(t, extra.ID)

	// New bundle drops camp-extra while keeping the default template.
	b := baseBundle()
	b.Templates = []models.BundleTemplate{b.Templates[0], b.Templates[2]}
	report, err := c.SyncTemplates(b)
	if err != nil {
		t.Fatalf("removal sync: %v", err)
	}

	if len(report.Deleted) != 1 || report.Deleted[0].Key != "camp-extra" {
		t.Fatalf("unexpected deletions: %+v", report.Deleted)
	}
	if len(report.Reassigned) != 1 {
		t.Fatalf("expected one reassignment, got %+v", report.Reassigned)
	}
	r := report.Reassigned[0]
	if r.FromTemplateID != extra.ID || r.ToTemplateID != def.ID || r.Campaigns != 1 {
		t.Fatalf("unexpected reassignment: %+v", r)
	}
	if got := c.campaignTemplateID(t, campID); got != def.ID {
		t.Fatalf("campaign now points at %d, want default %d", got, def.ID)
	}

	// The removed row is gone.
	var n int
	c.db.Get(&n, "SELECT COUNT(*) FROM templates WHERE template_key=$1", "camp-extra")
	if n != 0 {
		t.Fatal("removed managed template still exists")
	}
}

func TestSyncRollsBackWhenNoSuccessor(t *testing.T) {
	c := newTestCore(t)

	if _, err := c.SyncTemplates(baseBundle()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	extra := c.templateByKey(t, "camp-extra")
	campID := c.insertCampaign(t, extra.ID)

	// Remove every default so the removed template has no successor.
	if _, err := c.db.Exec("UPDATE templates SET is_default=false"); err != nil {
		t.Fatalf("clearing defaults: %v", err)
	}

	// Bundle without any default that also removes camp-extra and rewrites
	// camp-default; the whole batch must roll back.
	b := models.TemplateBundle{Templates: []models.BundleTemplate{
		{Key: "camp-default", Name: "Default campaign", Type: models.TemplateTypeCampaign, Body: campBody + "<!-- v2 -->"},
		{Key: "tx-welcome", Name: "Welcome", Type: models.TemplateTypeTx, Subject: "s", Body: "<p>x</p>"},
	}}
	_, err := c.SyncTemplates(b)
	wantHTTPStatus(t, err, http.StatusBadRequest)

	// Nothing changed: extra still exists, campaign still references it, and
	// the in-batch update of camp-default was rolled back too.
	if got := c.campaignTemplateID(t, campID); got != extra.ID {
		t.Fatalf("campaign reference changed after rollback: %d", got)
	}
	still := c.templateByKey(t, "camp-extra")
	if still.ID != extra.ID {
		t.Fatal("removed template was deleted despite rollback")
	}
	def := c.templateByKey(t, "camp-default")
	if strings.Contains(def.Body, "v2") {
		t.Fatal("batch update was not rolled back")
	}
	var defaults int
	c.db.Get(&defaults, "SELECT COUNT(*) FROM templates WHERE is_default=true")
	if defaults != 0 {
		t.Fatalf("default flags changed after rollback: %d", defaults)
	}
}

func TestSyncDefaultTakeover(t *testing.T) {
	c := newTestCore(t)

	if _, err := c.SyncTemplates(baseBundle()); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Move the default from camp-default to camp-extra.
	b := baseBundle()
	b.Templates[0].IsDefault = false
	b.Templates[1].IsDefault = true
	if _, err := c.SyncTemplates(b); err != nil {
		t.Fatalf("default handover sync: %v", err)
	}
	if c.templateByKey(t, "camp-extra").IsDefault != true {
		t.Fatal("camp-extra should now be the default")
	}
	if c.templateByKey(t, "camp-default").IsDefault != false {
		t.Fatal("camp-default should no longer be the default")
	}

	// The old default can now be removed safely; campaigns on it move to the
	// new default.
	campID := c.insertCampaign(t, c.templateByKey(t, "camp-default").ID)
	b.Templates = []models.BundleTemplate{b.Templates[1], b.Templates[2]}
	report, err := c.SyncTemplates(b)
	if err != nil {
		t.Fatalf("removal after handover: %v", err)
	}
	if len(report.Deleted) != 1 || report.Deleted[0].Key != "camp-default" {
		t.Fatalf("unexpected report: %+v", report)
	}
	newDef := c.templateByKey(t, "camp-extra")
	if got := c.campaignTemplateID(t, campID); got != newDef.ID {
		t.Fatalf("campaign not moved onto new default: %d", got)
	}
}

func TestSyncRejectsInvalidBundleWithoutWrites(t *testing.T) {
	c := newTestCore(t)

	b := baseBundle()
	b.Templates = append(b.Templates, b.Templates[0]) // duplicate key
	_, err := c.SyncTemplates(b)
	wantHTTPStatus(t, err, http.StatusBadRequest)

	if c.countTemplates(t) != 0 {
		t.Fatal("invalid bundle must not write any rows")
	}
}

func TestSyncRejectsTwoDefaults(t *testing.T) {
	c := newTestCore(t)

	b := baseBundle()
	b.Templates[1].IsDefault = true
	_, err := c.SyncTemplates(b)
	wantHTTPStatus(t, err, http.StatusBadRequest)
	if !strings.Contains(err.Error(), "camp-default") || !strings.Contains(err.Error(), "camp-extra") {
		t.Fatalf("error must name both default keys: %v", err)
	}
}
