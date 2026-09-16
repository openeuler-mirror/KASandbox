package catalog

import (
	"context"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/model"
)

type Catalog interface {
	Kind() string
	Snapshot(context.Context) (*model.CatalogData, error)
	Commit(context.Context, *model.CatalogData) error
}

// ImportSnapshot includes explicitly requested Build IDs even when legacy
// orphan rows have no Assignment. Source listing/export remains relation-based.
type ImportSnapshot interface {
	SnapshotForImport(context.Context, []string) (*model.CatalogData, error)
}
