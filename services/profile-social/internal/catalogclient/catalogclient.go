// Package catalogclient is profile-social's gRPC client for
// agrogram.catalog.v1.CatalogService, used for ListUserListings only:
// listings live in catalog and profile-social keeps no copy.
//
// Every call is made on the user's behalf: it carries `x-agrogram-caller:
// profile-social`, the incoming `x-request-id`, trace context and
// accept-language, and forwards the user headers unchanged (auth.proto), so
// catalog applies its own visibility rules as well. Each call has its own
// short timeout.
//
// TODO(catalog): catalog is not built yet. Until it runs, the address is
// empty (the service answers empty pages) or the calls fail with UNAVAILABLE,
// which this package reports as domain.ErrDependencyUnavailable so that the
// service degrades to an empty page.
package catalogclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	catalogv1 "github.com/tbilol/dxrl/gen/go/catalog/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	profilesocialv1 "github.com/tbilol/dxrl/gen/go/profilesocial/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/tbilol/dxrl/services/profile-social/internal/apperr"
	"github.com/tbilol/dxrl/services/profile-social/internal/auth"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// Client calls catalog.
type Client struct {
	conn    *grpc.ClientConn
	api     catalogv1.CatalogServiceClient
	timeout time.Duration
}

// Dial returns a client for addr. The connection is lazy (grpc-go connects on
// the first call), so catalog being down does not stop profile-social from
// starting.
//
// TODO(security): the local stack is plaintext inside the Docker network.
// Service-to-service TLS (or a mesh identity) belongs here once the platform
// has it; the `x-agrogram-caller` header is trusted until then.
func Dial(addr string, timeout time.Duration) (*Client, error) {
	if addr == "" {
		return nil, errors.New("catalog address is empty")
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("catalog client %q: %w", addr, err)
	}
	return &Client{conn: conn, api: catalogv1.NewCatalogServiceClient(conn), timeout: orDefault(timeout)}, nil
}

// New wraps an existing CatalogServiceClient (tests, or a shared connection).
func New(api catalogv1.CatalogServiceClient, timeout time.Duration) *Client {
	return &Client{api: api, timeout: orDefault(timeout)}
}

func orDefault(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 2 * time.Second
	}
	return timeout
}

// Close closes the connection.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// ListUserListings calls catalog ListListings with an owner filter and maps
// the listings to profile-social's ListingCard. The page token is catalog's
// and passes through unchanged.
func (c *Client) ListUserListings(ctx context.Context, p auth.Principal, q domain.ListingQuery) (domain.ListingPage, error) {
	ctx, cancel := c.call(ctx, p)
	defer cancel()
	resp, err := c.api.ListListings(ctx, &catalogv1.ListListingsRequest{
		Filter: &catalogv1.ListingFilter{
			OwnerId:  q.OwnerID.String(),
			Kind:     q.Kind,
			Statuses: q.Statuses,
		},
		Page: &commonv1.PageRequest{PageSize: q.Page.Size, PageToken: q.Page.Token},
	})
	if err != nil {
		return domain.ListingPage{}, wrap("ListListings", err)
	}
	out := domain.ListingPage{
		Listings:      make([]*profilesocialv1.ListingCard, 0, len(resp.GetListings())),
		NextPageToken: resp.GetPage().GetNextPageToken(),
	}
	for _, l := range resp.GetListings() {
		out.Listings = append(out.Listings, Card(l))
	}
	return out, nil
}

// Card maps a catalog listing to the compact card a profile shows. The cover
// is the first photo, or the first media (a video, whose thumbnail is its
// poster) when there is no photo.
func Card(l *catalogv1.Listing) *profilesocialv1.ListingCard {
	card := &profilesocialv1.ListingCard{
		ListingId:         l.GetId(),
		Kind:              l.GetKind(),
		Product:           l.GetProduct(),
		Quantity:          l.GetQuantity(),
		RemainingQuantity: l.GetRemainingQuantity(),
		Price:             l.GetPrice(),
		Region:            l.GetAddress().GetRegion(),
		District:          l.GetAddress().GetDistrict(),
		Status:            l.GetStatus(),
		CreatedAt:         l.GetCreatedAt(),
	}
	for _, m := range l.GetMedia() {
		if m.GetType() == commonv1.MediaType_MEDIA_TYPE_IMAGE {
			card.Cover = m
			break
		}
	}
	if card.Cover == nil && len(l.GetMedia()) > 0 {
		card.Cover = l.GetMedia()[0]
	}
	return card
}

// call builds the outgoing context: the metadata of a call on the user's
// behalf plus the per-call timeout. The parent context's deadline still
// applies.
func (c *Client) call(ctx context.Context, p auth.Principal) (context.Context, context.CancelFunc) {
	ctx = metadata.NewOutgoingContext(ctx, p.OutgoingMetadata())
	return context.WithTimeout(ctx, c.timeout)
}

// wrap turns a failed catalog call into profile-social's error model. When
// catalog cannot answer (UNAVAILABLE, including connection refused and name
// resolution failures, DEADLINE_EXCEEDED, UNIMPLEMENTED while it is a stub,
// or a transport error) the error is DEPENDENCY_UNAVAILABLE and wraps
// domain.ErrDependencyUnavailable; the service degrades on it. catalog's own
// errors (a bad page token, permission denied) keep their code.
func wrap(method string, err error) error {
	st, ok := status.FromError(err)
	if !ok || errors.Is(err, context.DeadlineExceeded) {
		return unavailable(method, err)
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Unimplemented:
		return unavailable(method, err)
	}
	e := apperr.From(err)
	e.Cause = fmt.Errorf("catalog %s: %w", method, err)
	return e
}

func unavailable(method string, err error) error {
	e := apperr.New(commonv1.ErrorReason_ERROR_REASON_DEPENDENCY_UNAVAILABLE,
		"catalog is unavailable", "dependency", "catalog")
	e.Cause = fmt.Errorf("catalog %s: %w: %w", method, domain.ErrDependencyUnavailable, err)
	return e
}
