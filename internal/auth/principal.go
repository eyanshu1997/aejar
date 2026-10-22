package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"strings"

	"github.com/eyanshu1997/aejar/internal/canon"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Kind is who is calling.
type Kind int

const (
	Anonymous Kind = iota
	Owner
	Session
)

// Principal is the authenticated caller.
type Principal struct {
	Kind Kind
	// Name is the audit actor: "owner:<how>" or "session:<task id>".
	Name      string
	TaskID    string
	Profile   string
	Sensitive bool
	TokenHash string
}

type ctxKey struct{}

// From returns the principal stored by the interceptor.
func From(ctx context.Context) Principal {
	p, _ := ctx.Value(ctxKey{}).(Principal)
	return p
}

// WithPrincipal stores p in ctx (used by the interceptor and tests).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// NewToken returns a random bearer token.
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Authenticator resolves bearer tokens and Access logins to principals.
type Authenticator struct {
	DB             *sql.DB
	OwnerTokenHash string // sha256 hex of the owner token
	OwnerEmails    []string
	OIDC           *OIDCVerifier // nil disables browser login
}

// IDTokenHeader carries the Google ID token nginx takes from oauth2-proxy,
// lowercased as gRPC metadata. nginx overwrites any client-sent copy.
const IDTokenHeader = "x-id-token"

// Resolve identifies the caller from gRPC metadata.
func (a *Authenticator) Resolve(ctx context.Context) Principal {
	md, _ := metadata.FromIncomingContext(ctx)
	if tok := bearer(md); tok != "" {
		h := canon.HashBytes([]byte(tok))
		if a.OwnerTokenHash != "" && subtle.ConstantTimeCompare([]byte(h), []byte(a.OwnerTokenHash)) == 1 {
			return Principal{Kind: Owner, Name: "owner:token"}
		}
		var p Principal
		var sensitive int
		err := a.DB.QueryRowContext(ctx, `SELECT task_id, profile, sensitive FROM sessions
			WHERE token_hash = ? AND revoked_at IS NULL`, h).Scan(&p.TaskID, &p.Profile, &sensitive)
		if err == nil {
			p.Kind, p.Name, p.Sensitive, p.TokenHash = Session, "session:"+p.TaskID, sensitive == 1, h
			return p
		}
	}
	if a.OIDC != nil {
		if vals := md.Get(IDTokenHeader); len(vals) > 0 {
			if email, err := a.OIDC.Email(vals[0]); err == nil {
				for _, o := range a.OwnerEmails {
					if strings.EqualFold(o, email) {
						return Principal{Kind: Owner, Name: "owner:" + email}
					}
				}
			}
		}
	}
	return Principal{Kind: Anonymous, Name: "anonymous"}
}

func bearer(md metadata.MD) string {
	for _, v := range md.Get("authorization") {
		if tok, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(tok)
		}
	}
	return ""
}

// sessionMethods are the only RPCs a session principal may call.
var sessionMethods = map[string]bool{
	"/aejar.v1.HookService/ClaudeHook":          true,
	"/aejar.v1.ApprovalService/RequestApproval": true,
	"/aejar.v1.ApprovalService/GetApproval":     true,
}

// Unary is the gRPC interceptor enforcing who may call what.
func (a *Authenticator) Unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	p := a.Resolve(ctx)
	switch {
	case p.Kind == Owner:
	case p.Kind == Session && sessionMethods[info.FullMethod]:
	case p.Kind == Session:
		return nil, status.Error(codes.PermissionDenied, "sessions may not call "+info.FullMethod)
	default:
		return nil, status.Error(codes.Unauthenticated, "authentication required")
	}
	return handler(WithPrincipal(ctx, p), req)
}
