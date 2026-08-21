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
