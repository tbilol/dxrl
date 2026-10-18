package identityclient

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	identityv1 "github.com/tbilol/dxrl/gen/go/identity/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/apperr"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/auth"
)

// stubIdentity is an in-process IdentityService client: it records the
// outgoing metadata and answers from its fields.
type stubIdentity struct {
	identityv1.IdentityServiceClient
	md      metadata.MD
	account *identityv1.Account
	err     error
	delay   time.Duration
}

func (s *stubIdentity) record(ctx context.Context) error {
	s.md, _ = metadata.FromOutgoingContext(ctx)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
	return s.err
}

func (s *stubIdentity) CreateAccount(ctx context.Context, _ *identityv1.CreateAccountRequest, _ ...grpc.CallOption) (*identityv1.CreateAccountResponse, error) {
	if err := s.record(ctx); err != nil {
		return nil, err
	}
	return &identityv1.CreateAccountResponse{Account: s.account}, nil
}

func (s *stubIdentity) GetAccount(ctx context.Context, _ *identityv1.GetAccountRequest, _ ...grpc.CallOption) (*identityv1.GetAccountResponse, error) {
	if err := s.record(ctx); err != nil {
		return nil, err
	}
	return &identityv1.GetAccountResponse{Account: s.account}, nil
}

func activeAccount(id uuid.UUID) *identityv1.Account {
	return &identityv1.Account{
		Id:     id.String(),
		Roles:  []commonv1.Role{commonv1.Role_ROLE_ADMIN},
		Status: identityv1.AccountStatus_ACCOUNT_STATUS_ACTIVE,
	}
}

func TestCreateAccountSendsCallerMetadata(t *testing.T) {
	id := uuid.New()
	stub := &stubIdentity{account: activeAccount(id)}
	client := New(stub, time.Second)

	p := auth.Principal{RequestID: "req-1", Traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}
	acct, err := client.CreateAccount(context.Background(), p, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if acct.ID != id || !acct.Active() {
		t.Fatalf("account = %+v", acct)
	}
	if got := acct.RoleNames(); len(got) != 1 || got[0] != "ADMIN" {
		t.Fatalf("role names = %v, want the x-agrogram-roles form", got)
	}
	// identity accepts CreateAccount from auth-gateway only.
	if got := stub.md.Get(auth.HeaderCaller); len(got) != 1 || got[0] != auth.ServiceName {
		t.Fatalf("%s = %v", auth.HeaderCaller, got)
	}
	if got := stub.md.Get(auth.HeaderRequestID); len(got) != 1 || got[0] != "req-1" {
		t.Fatalf("%s = %v", auth.HeaderRequestID, got)
	}
	if len(stub.md.Get(auth.HeaderTraceparent)) != 1 {
		t.Fatalf("traceparent = %v", stub.md.Get(auth.HeaderTraceparent))
	}
}

// An unreachable identity must become UNAVAILABLE / DEPENDENCY_UNAVAILABLE,
// never an internal error and never a hang.
func TestUnavailableIdentity(t *testing.T) {
	for name, stub := range map[string]*stubIdentity{
		"connection refused": {err: status.Error(codes.Unavailable, "connection refused")},
		"deadline exceeded":  {err: status.Error(codes.DeadlineExceeded, "too slow")},
		"timeout":            {delay: 50 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			client := New(stub, 10*time.Millisecond)
			_, err := client.GetAccount(context.Background(), auth.Principal{RequestID: "r"}, uuid.New())
			ae := apperr.From(err)
			if ae.Code != codes.Unavailable ||
				ae.Reason != commonv1.ErrorReason_ERROR_REASON_DEPENDENCY_UNAVAILABLE {
				t.Fatalf("error = %s / %s (%v)", ae.Code, ae.Reason, err)
			}
			if ae.Metadata["dependency"] != "identity" {
				t.Fatalf("metadata = %v", ae.Metadata)
			}
		})
	}
}

// identity's own errors keep their meaning (it already answers with a
// common.v1.Error).
func TestIdentityErrorsArePassedThrough(t *testing.T) {
	stub := &stubIdentity{err: status.Error(codes.NotFound, "account not found")}
	_, err := New(stub, time.Second).GetAccount(context.Background(), auth.Principal{}, uuid.New())
	if ae := apperr.From(err); ae.Code != codes.NotFound {
		t.Fatalf("error = %s (%v)", ae.Code, err)
	}
}

func TestInvalidAccountID(t *testing.T) {
	stub := &stubIdentity{account: &identityv1.Account{Id: "not-a-uuid"}}
	_, err := New(stub, time.Second).GetAccount(context.Background(), auth.Principal{}, uuid.New())
	if ae := apperr.From(err); ae.Code != codes.Internal {
		t.Fatalf("error = %s (%v)", ae.Code, err)
	}
}

func TestDialRequiresAnAddress(t *testing.T) {
	if _, err := Dial("", time.Second); err == nil {
		t.Fatal("want an error")
	}
}

func TestAccountActive(t *testing.T) {
	for status, want := range map[identityv1.AccountStatus]bool{
		identityv1.AccountStatus_ACCOUNT_STATUS_ACTIVE:      true,
		identityv1.AccountStatus_ACCOUNT_STATUS_BLOCKED:     false,
		identityv1.AccountStatus_ACCOUNT_STATUS_DELETED:     false,
		identityv1.AccountStatus_ACCOUNT_STATUS_UNSPECIFIED: false,
	} {
		if got := (Account{Status: status}).Active(); got != want {
			t.Fatalf("Active(%s) = %v, want %v", status, got, want)
		}
	}
	// ROLE_UNSPECIFIED never reaches a token.
	names := Account{Roles: []commonv1.Role{commonv1.Role_ROLE_UNSPECIFIED, commonv1.Role_ROLE_ADMIN}}.RoleNames()
	if len(names) != 1 || names[0] != "ADMIN" {
		t.Fatalf("role names = %v", names)
	}
}
