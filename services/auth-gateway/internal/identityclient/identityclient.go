// Package identityclient is auth-gateway's gRPC client for
// agrogram.identity.v1.IdentityService: the account store.
//
// auth-gateway owns credentials and sessions, never accounts. It calls
// identity in exactly two places: CreateAccount on a first sign-in, and
// GetAccount whenever it issues or refreshes tokens, so the access token
// carries live roles (back/README.md, "auth-gateway
// -> identity when it issues tokens").
//
// Every call carries `x-agrogram-caller: auth-gateway` (identity's
// CreateAccount accepts that caller only) plus the incoming `x-request-id` and
// trace context, and has its own short timeout: a slow identity turns into
// UNAVAILABLE / DEPENDENCY_UNAVAILABLE, never into a hanging sign-in.
package identityclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	identityv1 "github.com/tbilol/dxrl/gen/go/identity/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/apperr"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/auth"
)

// Account is the part of agrogram.identity.v1.Account auth-gateway needs: the
// claims of an access token and the check that the account may be used.
type Account struct {
	ID     uuid.UUID
	Roles  []commonv1.Role
	Status identityv1.AccountStatus
}

// Active reports whether the account may sign in (BLOCKED and DELETED may not).
func (a Account) Active() bool {
	return a.Status == identityv1.AccountStatus_ACCOUNT_STATUS_ACTIVE
}

// RoleNames returns the role names without the `ROLE_` prefix, the form used
// by the `roles` claim and by `x-agrogram-roles`.
func (a Account) RoleNames() []string {
	names := make([]string, 0, len(a.Roles))
	for _, r := range a.Roles {
		if r == commonv1.Role_ROLE_UNSPECIFIED {
			continue
		}
		names = append(names, trimRolePrefix(r.String()))
	}
	return names
}

func trimRolePrefix(s string) string {
	const prefix = "ROLE_"
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

// Client calls identity.
type Client struct {
	conn    *grpc.ClientConn
	api     identityv1.IdentityServiceClient
	timeout time.Duration
}

// Dial returns a client for addr. The connection is lazy (grpc-go connects on
// the first call), so identity being down does not stop auth-gateway from
// starting; the calls fail with DEPENDENCY_UNAVAILABLE until it is back.
//
// TODO(security): the local stack is plaintext inside the Docker network.
// Service-to-service TLS (or a mesh identity) belongs here once the platform
// has it; the `x-agrogram-caller` header is trusted until then.
func Dial(addr string, timeout time.Duration) (*Client, error) {
	if addr == "" {
		return nil, errors.New("identity address is empty")
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("identity client %q: %w", addr, err)
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Client{conn: conn, api: identityv1.NewIdentityServiceClient(conn), timeout: timeout}, nil
}

// New wraps an existing IdentityServiceClient (tests, or a shared connection).
func New(api identityv1.IdentityServiceClient, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Client{api: api, timeout: timeout}
}

// Close closes the connection.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// CreateAccount creates the account of a first sign-in. requestID is derived
// from the sign-in identity (a UUIDv5), so a retried first sign-in returns the
// same account instead of creating a second one.
func (c *Client) CreateAccount(ctx context.Context, p auth.Principal, requestID uuid.UUID) (Account, error) {
	ctx, cancel := c.call(ctx, p)
	defer cancel()
	resp, err := c.api.CreateAccount(ctx, &identityv1.CreateAccountRequest{RequestId: requestID.String()})
	if err != nil {
		return Account{}, wrap("CreateAccount", err)
	}
	return account(resp.GetAccount())
}

// GetAccount reads an account's live roles and status.
func (c *Client) GetAccount(ctx context.Context, p auth.Principal, userID uuid.UUID) (Account, error) {
	ctx, cancel := c.call(ctx, p)
	defer cancel()
	resp, err := c.api.GetAccount(ctx, &identityv1.GetAccountRequest{UserId: userID.String()})
	if err != nil {
		return Account{}, wrap("GetAccount", err)
	}
	return account(resp.GetAccount())
}

// call builds the outgoing context: the caller metadata identity expects plus
// the per-call timeout. The parent context's own deadline still applies.
func (c *Client) call(ctx context.Context, p auth.Principal) (context.Context, context.CancelFunc) {
	ctx = metadata.NewOutgoingContext(ctx, p.OutgoingMetadata())
	return context.WithTimeout(ctx, c.timeout)
}

func account(pb *identityv1.Account) (Account, error) {
	id, err := uuid.Parse(pb.GetId())
	if err != nil {
		return Account{}, apperr.Internal(fmt.Errorf("identity returned an invalid account id %q: %w", pb.GetId(), err))
	}
	return Account{
		ID:     id,
		Roles:  pb.GetRoles(),
		Status: pb.GetStatus(),
	}, nil
}

// wrap turns a failed identity call into auth-gateway's error model: a
// transport failure or a timeout is DEPENDENCY_UNAVAILABLE, and identity's own
// errors keep their code (identity already answers with a common.v1.Error).
func wrap(method string, err error) error {
	st, ok := status.FromError(err)
	if !ok || st.Code() == codes.Unavailable || st.Code() == codes.DeadlineExceeded ||
		errors.Is(err, context.DeadlineExceeded) {
		e := apperr.New(commonv1.ErrorReason_ERROR_REASON_DEPENDENCY_UNAVAILABLE,
			"identity is unavailable", "dependency", "identity")
		e.Cause = fmt.Errorf("identity %s: %w", method, err)
		return e
	}
	e := apperr.From(err)
	e.Cause = fmt.Errorf("identity %s: %w", method, err)
	return e
}
