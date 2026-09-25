package service

import (
	"context"
	"fmt"

	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/catalog/internal/auth"
	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

// ListProductTypes returns the product reference data of category
// (UNSPECIFIED: all) in display order, retired products included (flagged)
// so old listings keep their labels. Anyone may call it; the names come in
// both languages, so accept-language is not needed.
func (s *Service) ListProductTypes(ctx context.Context, _ auth.Principal, category commonv1.ProductCategory) ([]domain.ProductType, error) {
	var out []domain.ProductType
	err := s.store.Run(ctx, func(q Queries) (err error) {
		out, err = q.ListProductTypes(ctx, category)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list product types: %w", err)
	}
	return out, nil
}
