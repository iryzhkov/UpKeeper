//go:build !linux

package author

import (
	"context"
	"errors"
)

func AcquireCatalogOriginalImportSnapshot(context.Context, string) (*CatalogOriginalImportSnapshot, error) {
	return nil, errors.New("baseline original custody requires Linux")
}
func AcquireCatalogGeneratedBaselineSnapshot(context.Context, string, string) (*CatalogGeneratedBaselineSnapshot, error) {
	return nil, errors.New("baseline generated custody requires Linux")
}
func AcquireCatalogHistoricalTemplateSnapshot(context.Context, string) (*CatalogHistoricalTemplateSnapshot, error) {
	return nil, errors.New("baseline historical custody requires Linux")
}
func inspectCatalogBaselineLocked(context.Context, *CatalogSnapshot, *CatalogOriginalImportSnapshot, *CatalogGeneratedBaselineSnapshot, *CatalogHistoricalTemplateSnapshot, CatalogGuidanceAudit, CatalogProducerAudit) (CatalogBaselineAudit, error) {
	return CatalogBaselineAudit{}, errors.New("baseline audit requires Linux")
}
