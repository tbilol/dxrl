package postgres

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

// sqlArgs collects query parameters and names their placeholders.
type sqlArgs []any

func (a *sqlArgs) add(v any) string {
	*a = append(*a, v)
	return "$" + strconv.Itoa(len(*a))
}

// sortKey is one ORDER BY term of a listing query and its value in the
// cursor.
type sortKey struct {
	expr  string
	desc  bool
	value any
	cast  string // placeholder cast, e.g. "::real"
}

// findListingsSQL builds the listing query of ListListings and
// SearchListings: the criteria as WHERE conditions, the text rank, the sort
// (with created_at and id as tie-breakers) and the keyset condition after
// the cursor. Every value is a parameter; the only literals are the fixed
// enum names of the status condition and the negotiable price sort key.
func findListingsSQL(q domain.ListingQuery) (string, []any, error) {
	c := q.Criteria
	var (
		args  sqlArgs
		where []string
	)

	statuses := make([]string, len(c.Statuses))
	for i, st := range c.Statuses {
		s, err := listingStatuses.text(st)
		if err != nil {
			return "", nil, err
		}
		statuses[i] = s
	}
	switch len(statuses) {
	case 0:
		return "", nil, fmt.Errorf("listing query without statuses")
	case 1:
		// A literal, so the planner can use the partial indexes on ACTIVE
		// listings with generic plans too. The value is a fixed enum name.
		where = append(where, "l.status = '"+statuses[0]+"'")
	default:
		where = append(where, "l.status = ANY("+args.add(statuses)+")")
	}
	if c.Kind != 0 {
		k, err := kinds.text(c.Kind)
		if err != nil {
			return "", nil, err
		}
		where = append(where, "l.kind = "+args.add(k))
	}
	if c.OwnerID != uuid.Nil {
		where = append(where, "l.owner_id = "+args.add(c.OwnerID))
	}
	if len(c.Products) > 0 {
		ids := make([]int32, len(c.Products))
		for i, p := range c.Products {
			ids[i] = int32(p)
		}
		where = append(where, "l.product_type = ANY("+args.add(ids)+")")
	}
	if len(c.Regions) > 0 {
		names := make([]string, len(c.Regions))
		for i, r := range c.Regions {
			n, err := regions.text(r)
			if err != nil {
				return "", nil, err
			}
			names[i] = n
		}
		where = append(where, "l.region = ANY("+args.add(names)+")")
	}
	if c.District != "" {
		where = append(where, "lower(l.district) = lower("+args.add(c.District)+")")
	}
	if c.MinQuantityGrams > 0 {
		where = append(where, "l.quantity_grams >= "+args.add(c.MinQuantityGrams))
	}
	if c.MinPricePerTonne != nil {
		where = append(where, "l.price_per_tonne_minor >= "+args.add(*c.MinPricePerTonne))
	}
	if c.MaxPricePerTonne != nil {
		where = append(where, "l.price_per_tonne_minor <= "+args.add(*c.MaxPricePerTonne))
	}
	rank := "0::real"
	if len(c.Terms) > 0 {
		tsquery := "to_tsquery('simple', " + args.add(prefixQuery(c.Terms)) + ")"
		where = append(where, "l.search_document @@ "+tsquery)
		rank = "ts_rank(l.search_document, " + tsquery + ")"
	}

	var cur domain.Cursor // zero values on the first page (no keyset condition)
	if q.After != nil {
		cur = *q.After
	}
	created := sortKey{expr: "l.created_at", desc: true, value: cur.CreatedAt}
	id := sortKey{expr: "l.id", desc: true, value: cur.ID}
	var keys []sortKey
	switch q.Sort {
	case domain.SortCreatedAt:
		created.desc, id.desc = q.Desc, q.Desc
		keys = []sortKey{created, id}
	case domain.SortPrice:
		// Negotiable listings (no price) get a key that sorts last in both
		// directions; the service puts the same key into cursors.
		expr := fmt.Sprintf("COALESCE(l.price_per_tonne_minor, %d)", domain.NegotiableSortKey(q.Desc))
		keys = []sortKey{{expr: expr, desc: q.Desc, value: cur.Key}, created, id}
	case domain.SortQuantity:
		keys = []sortKey{{expr: "l.quantity_grams", desc: q.Desc, value: cur.Key}, created, id}
	case domain.SortRelevance:
		keys = []sortKey{{expr: rank, desc: true, value: cur.Rank, cast: "::real"}, created, id}
	default:
		return "", nil, fmt.Errorf("unknown listing sort %q", q.Sort)
	}
	if q.After != nil {
		where = append(where, keyset(keys, &args))
	}

	order := make([]string, len(keys))
	for i, k := range keys {
		order[i] = k.expr + direction(k.desc)
	}
	sql := "SELECT " + listingCols + ", " + rank + " AS rank\nFROM listings l\nWHERE " +
		strings.Join(where, "\n  AND ") + "\nORDER BY " + strings.Join(order, ", ") +
		"\nLIMIT " + args.add(q.Limit)
	return sql, args, nil
}

// keyset is the condition "strictly after the cursor" for the sort keys. When
// every key has the same direction it is one row comparison, which a btree
// index in that order can use as a bound; otherwise it is the expanded
// lexicographic OR.
func keyset(keys []sortKey, args *sqlArgs) string {
	vals := make([]string, len(keys))
	exprs := make([]string, len(keys))
	sameDirection := true
	for i, k := range keys {
		vals[i] = args.add(k.value) + k.cast
		exprs[i] = k.expr
		sameDirection = sameDirection && k.desc == keys[0].desc
	}
	if sameDirection {
		return "(" + strings.Join(exprs, ", ") + ") " + after(keys[0].desc) + " (" + strings.Join(vals, ", ") + ")"
	}
	ors := make([]string, len(keys))
	for i, k := range keys {
		ands := make([]string, 0, i+1)
		for j := range i {
			ands = append(ands, exprs[j]+" = "+vals[j])
		}
		ands = append(ands, k.expr+" "+after(k.desc)+" "+vals[i])
		ors[i] = "(" + strings.Join(ands, " AND ") + ")"
	}
	return "(" + strings.Join(ors, " OR ") + ")"
}

func after(desc bool) string {
	if desc {
		return "<"
	}
	return ">"
}

func direction(desc bool) string {
	if desc {
		return " DESC"
	}
	return " ASC"
}

// prefixQuery turns normalized search terms into to_tsquery syntax: every
// term must match as a word prefix ('kart':* & 'urgut':*). Terms are letters
// and digits only (service.Terms); quotes and backslashes are escaped anyway,
// so no input can change the query's structure.
func prefixQuery(terms []string) string {
	parts := make([]string, len(terms))
	for i, t := range terms {
		t = strings.ReplaceAll(t, `\`, `\\`)
		t = strings.ReplaceAll(t, `'`, `''`)
		parts[i] = "'" + t + "':*"
	}
	return strings.Join(parts, " & ")
}
