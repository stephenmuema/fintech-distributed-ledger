package app

import (
	"context"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Chart (RE) is the chart of accounts as data: the built-in GL lines and categories plus the
// categories allocated at runtime by parameter maintenance. It is read per use rather than
// cached, so an authorised parameter change is visible to every instance immediately.
type Chart struct{ a *App }

// Load returns the full chart as seen by q (a transaction sees its own allocations).
func (c *Chart) Load(q store.Q) (domain.Chart, error) {
	rows, err := store.Select[domain.RuntimeCategory](q, "SELECT * FROM categories ORDER BY code")
	if err != nil {
		return domain.Chart{}, err
	}
	extra := make([]domain.CategoryDefinition, len(rows))
	for i, r := range rows {
		extra[i] = r.Definition()
	}
	return domain.BuiltInChart().WithCategories(extra), nil
}

// Get is the chart for reads.
func (c *Chart) Get(ctx context.Context) (domain.Chart, error) { return c.Load(c.a.read(ctx)) }

// GLFor resolves the GL line of any category, built-in or runtime.
func (c *Chart) GLFor(q store.Q, category int) (string, error) {
	chart, err := c.Load(q)
	if err != nil {
		return "", err
	}
	return chart.GLFor(category)
}

// Allocate takes the lowest free category in [first, last] for owner and records it.
func (c *Chart) Allocate(tx *store.Tx, first, last int, name, glCode, owner string) (int, error) {
	chart, err := c.Load(tx)
	if err != nil {
		return 0, err
	}
	if _, ok := domain.GLAccountByCode(glCode); !ok {
		return 0, domain.Rule("invalid_gl_line", "GL line %s is not in the chart of accounts.", glCode)
	}
	code, err := chart.NextFree(first, last)
	if err != nil {
		return 0, err
	}
	category := &domain.RuntimeCategory{ID: domain.NewID(), Code: code, Name: name, GLCode: glCode, Owner: owner, CreatedAt: tx.Now()}
	if err := tx.Insert("categories", category); err != nil {
		return 0, err
	}
	return code, tx.Audit("chart.category_allocated", "category", category.ID.String(), category)
}
