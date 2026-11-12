package core

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v5"
	null "gopkg.in/volatiletech/null.v6"
)

// templateSyncAdvisoryLock serializes concurrent bundle syncs so that two
// reconciliations cannot interleave their upserts/deletes. The value is an
// arbitrary fixed int64 key for Postgres' pg_advisory_xact_lock.
const templateSyncAdvisoryLock int64 = 88421763

// SyncTemplates reconciles the given template bundle against the DB in a
// single transaction:
//
//   - Bundle entries are upserted keyed by template_key, so an upgraded entry
//     updates the existing template instead of creating a new one.
//   - Managed templates (managed=true, template_key set) missing from the
//     bundle are removed, but only after campaigns referencing them are moved
//     onto the bundle's default campaign template. Manually created templates
//     (template_key IS NULL) are never touched.
//   - Any validation or SQL error rolls the whole batch back; the DB is left
//     exactly as it was before the call.
//   - Unchanged entries produce no writes, making an identical re-run a no-op.
func (c *Core) SyncTemplates(b models.TemplateBundle) (models.TemplateSyncReport, error) {
	report := models.NewTemplateSyncReport()

	// Re-validate at the data layer so the batch can never be partially applied
	// even if a caller skips handler-level validation.
	if errs := b.Validate(); len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return report, echo.NewHTTPError(http.StatusBadRequest, strings.Join(msgs, "; "))
	}

	tx, err := c.db.Beginx()
	if err != nil {
		return report, echo.NewHTTPError(http.StatusInternalServerError,
			c.i18n.Ts("globals.messages.errorUpdating", "name", "{globals.terms.templates}", "error", pqErrMsg(err)))
	}
	defer func() { _ = tx.Rollback() }()

	// Serialize concurrent syncs for the lifetime of this transaction.
	if _, err := tx.Exec("SELECT pg_advisory_xact_lock($1)", templateSyncAdvisoryLock); err != nil {
		return report, echo.NewHTTPError(http.StatusInternalServerError,
			c.i18n.Ts("globals.messages.errorUpdating", "name", "{globals.terms.templates}", "error", pqErrMsg(err)))
	}

	// Load every bundle-managed template currently stored.
	var existing []models.Template
	if err := tx.Select(&existing, c.q.GetManagedTemplates); err != nil {
		return report, echo.NewHTTPError(http.StatusInternalServerError,
			c.i18n.Ts("globals.messages.errorFetching", "name", "{globals.terms.templates}", "error", pqErrMsg(err)))
	}
	byKey := make(map[string]models.Template, len(existing))
	for _, t := range existing {
		byKey[t.TemplateKey.String] = t
	}

	bundleKeys := make(map[string]struct{}, len(b.Templates))
	idByKey := make(map[string]int, len(b.Templates))

	// 1. Upsert bundle entries (create or in-place update, never duplicate).
	for _, bt := range b.Templates {
		key := strings.TrimSpace(bt.Key)
		bundleKeys[key] = struct{}{}

		subject := bt.Subject
		if bt.Type != models.TemplateTypeTx {
			// Subject lives on campaigns, not on their templates.
			subject = ""
		}

		if cur, ok := byKey[key]; ok {
			idByKey[key] = cur.ID
			if managedTemplateEquals(cur, bt, subject) {
				report.Unchanged = append(report.Unchanged, key)
				continue
			}

			if _, err := tx.Exec(c.q.UpdateManagedTemplate,
				cur.ID, bt.Name, bt.Type, subject, bt.Body, bt.BodySource); err != nil {
				return report, echo.NewHTTPError(http.StatusInternalServerError,
					c.i18n.Ts("globals.messages.errorUpdating", "name", "{globals.terms.template}", "error", pqErrMsg(err)))
			}
			report.Updated = append(report.Updated, models.TemplateSyncResult{Key: key, ID: cur.ID, Type: bt.Type})
			continue
		}

		var newID int
		if err := tx.Get(&newID, c.q.InsertManagedTemplate,
			bt.Name, bt.Type, subject, bt.Body, bt.BodySource, key); err != nil {
			return report, echo.NewHTTPError(http.StatusInternalServerError,
				c.i18n.Ts("globals.messages.errorCreating", "name", "{globals.terms.template}", "error", pqErrMsg(err)))
		}
		idByKey[key] = newID
		report.Created = append(report.Created, models.TemplateSyncResult{Key: key, ID: newID, Type: bt.Type})
	}

	// 2. Hand over the default flag to the bundle's default campaign template.
	if defaultKey, ok := b.DefaultKey(); ok {
		defID, ok := idByKey[defaultKey]
		if !ok {
			return report, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("template key %q: declared as default but was not synced", defaultKey))
		}

		curDefaultID, err := c.getDefaultTemplateIDTx(tx)
		if err != nil {
			return report, err
		}
		if curDefaultID != defID {
			if _, err := tx.Exec(c.q.SetDefaultTemplateSync, defID); err != nil {
				return report, echo.NewHTTPError(http.StatusInternalServerError,
					c.i18n.Ts("globals.messages.errorUpdating", "name", "{globals.terms.template}", "error", pqErrMsg(err)))
			}
		}
	}

	// 3. Remove managed templates that disappeared from the bundle, after
	// moving any referencing campaigns onto the successor default template.
	for _, cur := range existing {
		key := cur.TemplateKey.String
		if _, ok := bundleKeys[key]; ok {
			continue
		}

		successor, err := c.resolveSuccessorTemplateIDTx(tx, b, idByKey)
		if err != nil {
			return report, err
		}
		if successor == 0 {
			return report, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("template key %q: cannot remove because no default campaign template exists to take over campaigns referencing it", key))
		}

		res, err := tx.Exec(c.q.ReassignCampaignTpl, cur.ID, successor)
		if err != nil {
			return report, echo.NewHTTPError(http.StatusInternalServerError,
				c.i18n.Ts("globals.messages.errorUpdating", "name", "{globals.terms.campaign}", "error", pqErrMsg(err)))
		}
		if n, _ := res.RowsAffected(); n > 0 {
			report.Reassigned = append(report.Reassigned, models.CampaignReassignment{
				FromTemplateID: cur.ID,
				ToTemplateID:   successor,
				Campaigns:      int(n),
			})
		}

		delRes, err := tx.Exec(c.q.DeleteManagedTemplate, cur.ID)
		if err != nil {
			return report, echo.NewHTTPError(http.StatusInternalServerError,
				c.i18n.Ts("globals.messages.errorDeleting", "name", "{globals.terms.template}", "error", pqErrMsg(err)))
		}
		if n, _ := delRes.RowsAffected(); n == 0 {
			return report, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("template key %q: cannot remove a default template; declare a replacement default in the bundle", key))
		}
		report.Deleted = append(report.Deleted, models.TemplateSyncResult{Key: key, ID: cur.ID, Type: cur.Type})
	}

	if err := tx.Commit(); err != nil {
		return report, echo.NewHTTPError(http.StatusInternalServerError,
			c.i18n.Ts("globals.messages.errorUpdating", "name", "{globals.terms.templates}", "error", pqErrMsg(err)))
	}

	return report, nil
}

// resolveSuccessorTemplateID returns the template that should inherit campaigns
// from a removed managed template: the bundle's declared default when there is
// one, otherwise the current default template in the DB.
func (c *Core) resolveSuccessorTemplateIDTx(tx *sqlx.Tx, b models.TemplateBundle, idByKey map[string]int) (int, error) {
	if defaultKey, ok := b.DefaultKey(); ok {
		if id, ok := idByKey[defaultKey]; ok {
			return id, nil
		}
	}

	id, err := c.getDefaultTemplateIDTx(tx)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// getDefaultTemplateIDTx returns the current default template ID inside the
// given transaction, or 0 when no default is set.
func (c *Core) getDefaultTemplateIDTx(tx *sqlx.Tx) (int, error) {
	var id int
	if err := tx.Get(&id, c.q.GetDefaultTemplateID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, echo.NewHTTPError(http.StatusInternalServerError,
			c.i18n.Ts("globals.messages.errorFetching", "name", "{globals.terms.template}", "error", pqErrMsg(err)))
	}
	return id, nil
}

// managedTemplateEquals reports whether a stored managed template already
// matches the bundle entry, in which case no UPDATE is issued and an identical
// re-run produces no changes.
func managedTemplateEquals(t models.Template, b models.BundleTemplate, subject string) bool {
	return t.Name == b.Name &&
		t.Type == b.Type &&
		t.Subject == subject &&
		t.Body == b.Body &&
		nullStringEquals(t.BodySource, b.BodySource)
}

// nullStringEquals compares two nullable strings regardless of their package
// representation (both are null.v6 String values here).
func nullStringEquals(a, b null.String) bool {
	return a.Valid == b.Valid && a.String == b.String
}
