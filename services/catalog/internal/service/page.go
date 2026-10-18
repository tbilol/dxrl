package service

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

// Page sizes (back/README.md: 0 means 20, the maximum is 100).
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

func pageSize(n int32) int {
	if n <= 0 {
		return defaultPageSize
	}
	return min(int(n), maxPageSize)
}

// pageTokenVersion changes when the token layout does; older tokens are then
// rejected like any other foreign token.
const pageTokenVersion = 1

// pageToken is the content of an opaque page token: the fingerprint of the
// query it belongs to and the keyset position of the last listing returned.
type pageToken struct {
	Version     int     `json:"v"`
	Fingerprint string  `json:"f"`
	Key         int64   `json:"k,omitempty"` // price per tonne or quantity in grams
	Rank        float32 `json:"r,omitempty"` // text rank (float32 round-trips exactly through JSON)
	At          int64   `json:"t"`           // created_at, Unix microseconds
	ID          string  `json:"i"`
}

// fingerprint identifies a query for its page tokens: the operation (list or
// search), the normalized and authorized criteria, and the sort. A token is
// valid only for the query that produced it; the page size may change.
func fingerprint(op string, q domain.ListingQuery) string {
	c := q.Criteria
	opt := func(v *int64) string {
		if v == nil {
			return "-"
		}
		return strconv.FormatInt(*v, 10)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s|sort=%s|desc=%t|kind=%d|owner=%s|district=%q|min_grams=%d|min_price=%s|max_price=%s",
		op, q.Sort, q.Desc, c.Kind, c.OwnerID, strings.ToLower(c.District), c.MinQuantityGrams,
		opt(c.MinPricePerTonne), opt(c.MaxPricePerTonne))
	fmt.Fprintf(&b, "|products=%v|regions=%v|statuses=%v|terms=%q", c.Products, c.Regions, c.Statuses, c.Terms)
	sum := sha256.Sum256([]byte(b.String()))
	return base64.RawURLEncoding.EncodeToString(sum[:12])
}

func encodePageToken(fp string, c domain.Cursor) string {
	b, err := json.Marshal(pageToken{
		Version: pageTokenVersion, Fingerprint: fp, Key: c.Key, Rank: c.Rank,
		At: c.CreatedAt.UnixMicro(), ID: c.ID.String(),
	})
	if err != nil { // cannot happen: strings and numbers
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodePageToken parses a page token of the query with fingerprint fp; ""
// is the first page. A token of another query (other filter, text, sort or
// operation), of another layout version, or no token at all is
// INVALID_ARGUMENT.
func decodePageToken(token, fp string) (*domain.Cursor, error) {
	if token == "" {
		return nil, nil
	}
	bad := invalid("page.page_token",
		"page_token does not belong to this query; start again with an empty page_token", "page_token")
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, bad
	}
	var t pageToken
	if err := json.Unmarshal(raw, &t); err != nil || t.Version != pageTokenVersion || t.Fingerprint != fp {
		return nil, bad
	}
	id, err := uuid.Parse(t.ID)
	if err != nil {
		return nil, bad
	}
	return &domain.Cursor{Key: t.Key, Rank: t.Rank, CreatedAt: time.UnixMicro(t.At).UTC(), ID: id}, nil
}
