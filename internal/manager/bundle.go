package manager

import (
	"fmt"

	"github.com/knadh/listmonk/models"
)

// dummyBundleSubscriber feeds the dry-run render used to validate a bundle
// before it touches the DB.
var dummyBundleSubscriber = models.Subscriber{
	Email: "demo@listmonk.app",
	Name:  "Demo Subscriber",
	UUID:  dummyUUID,
}

const dummyBundleCampaignBody = `<p>Demo campaign body</p>`

// ValidateBundle compiles and dry-run renders every entry of a template
// bundle. It returns an error naming the first entry that fails to compile or
// render, allowing callers to reject the entire batch with a precise template
// key before any DB write happens.
func (m *Manager) ValidateBundle(b models.TemplateBundle) error {
	for _, t := range b.Templates {
		if err := m.validateBundleTemplate(t); err != nil {
			return fmt.Errorf("template key %q: %w", t.Key, err)
		}
	}
	return nil
}

// validateBundleTemplate runs the same compile/render pipeline as the template
// preview against dummy data, which surfaces both parse errors and execution
// time errors (for instance a template function called with bad arguments).
func (m *Manager) validateBundleTemplate(t models.BundleTemplate) error {
	switch t.Type {
	case models.TemplateTypeCampaign, models.TemplateTypeCampaignVisual:
		camp := models.Campaign{
			UUID:         dummyUUID,
			Name:         t.Name,
			Subject:      "Dummy campaign subject",
			FromEmail:    "dummy-campaign@listmonk.app",
			TemplateBody: t.Body,
			Body:         dummyBundleCampaignBody,
		}
		if err := camp.CompileTemplate(m.TemplateFuncs(&camp, models.CampPreview)); err != nil {
			return err
		}
		if _, err := m.NewCampaignMessage(&camp, dummyBundleSubscriber); err != nil {
			return err
		}
		return nil

	case models.TemplateTypeTx:
		tpl := models.Template{Type: t.Type, Subject: t.Subject, Body: t.Body}
		funcs := m.GenericTemplateFuncs(models.CampPreview)
		if err := tpl.Compile(funcs); err != nil {
			return err
		}
		msg := models.TxMessage{Subject: tpl.Subject}
		return msg.Render(dummyBundleSubscriber, &tpl, funcs)

	default:
		// Static bundle validation already rejects unknown types.
		return fmt.Errorf("unknown template type %q", t.Type)
	}
}
