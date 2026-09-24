package service

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/profile-social/internal/apperr"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
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

// cursorToken is the content of an opaque page token: the list and user it
// belongs to, and the keyset position (sort time in Unix microseconds and the
// tie-breaking id) of the last item returned.
type cursorToken struct {
	List string `json:"l"`
	User string `json:"u"`
	At   int64  `json:"t"`
	ID   string `json:"i"`
}

func encodeCursor(list string, user uuid.UUID, c domain.Cursor) string {
	b, err := json.Marshal(cursorToken{List: list, User: user.String(), At: c.At.UnixMicro(), ID: c.ID.String()})
	if err != nil { // cannot happen: strings and an int
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor parses a page token of list for user; "" is the first page. A
// token of another list, another user or no token at all is
// INVALID_ARGUMENT.
func decodeCursor(token, list string, user uuid.UUID) (*domain.Cursor, error) {
	if token == "" {
		return nil, nil
	}
	invalid := apperr.Validation(&commonv1.FieldViolation{
		Field:       "page.page_token",
		Description: "page_token does not belong to this list; start again with an empty page_token",
		Rule:        "page_token",
	})
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, invalid
	}
	var t cursorToken
	if err := json.Unmarshal(b, &t); err != nil || t.List != list || t.User != user.String() {
		return nil, invalid
	}
	id, err := uuid.Parse(t.ID)
	if err != nil {
		return nil, invalid
	}
	return &domain.Cursor{At: time.UnixMicro(t.At).UTC(), ID: id}, nil
}
