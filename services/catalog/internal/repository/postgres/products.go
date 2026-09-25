package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

const productTypeCols = `id, key, name_uz, name_ru, category, sort_order, is_retired`

func scanProductType(row pgx.Row) (domain.ProductType, error) {
	var (
		pt       domain.ProductType
		id       int32
		category string
	)
	if err := row.Scan(&id, &pt.Key, &pt.NameUZ, &pt.NameRU, &category, &pt.SortOrder, &pt.Retired); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pt, domain.ErrNotFound
		}
		return pt, err
	}
	pt.Type = commonv1.ProductType(id)
	var err error
	pt.Category, err = categories.value(category)
	return pt, err
}

func (q *queries) GetProductType(ctx context.Context, t commonv1.ProductType) (domain.ProductType, error) {
	return scanProductType(q.db.QueryRow(ctx, `SELECT `+productTypeCols+` FROM product_types WHERE id = $1`, int32(t)))
}

func (q *queries) ListProductTypes(ctx context.Context, category commonv1.ProductCategory) ([]domain.ProductType, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if category == commonv1.ProductCategory_PRODUCT_CATEGORY_UNSPECIFIED {
		rows, err = q.db.Query(ctx, `SELECT `+productTypeCols+` FROM product_types ORDER BY sort_order, id`)
	} else {
		var c string
		if c, err = categories.text(category); err != nil {
			return nil, err
		}
		rows, err = q.db.Query(ctx, `SELECT `+productTypeCols+` FROM product_types WHERE category = $1
ORDER BY sort_order, id`, c)
	}
	if err != nil {
		return nil, fmt.Errorf("list product types: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.ProductType, error) {
		return scanProductType(row)
	})
	if err != nil {
		return nil, fmt.Errorf("list product types: %w", err)
	}
	return out, nil
}
