package app

import (
	"context"
	"errors"
	"strings"

	"github.com/priyavratuniyal/tuskbase/internal/domain"
)

type Role string

const (
	RoleReader Role = "reader"
	RoleAgent  Role = "agent"
	RoleAdmin  Role = "admin"
)

const (
	AuthSourceLocalAPIKey    = "local-api-key"
	AuthSourceLocalSharedKey = "local-shared-key"
)

var ErrForbidden = errors.New("forbidden")

// Principal is the application-level identity contract. Local keys and API
// keys, and future OAuth flows should all resolve to this shape before use cases
// see them.
type Principal struct {
	Subject    string       `json:"subject"`
	Role       Role         `json:"role"`
	Actor      domain.Actor `json:"actor"`
	AuthSource string       `json:"auth_source"`
}

type principalContextKey struct{}

func ContextWithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	return p, ok
}

func (p Principal) CanRead() bool {
	switch p.Role {
	case RoleReader, RoleAgent, RoleAdmin:
		return true
	default:
		return false
	}
}

func (p Principal) CanWrite() bool {
	switch p.Role {
	case RoleAgent, RoleAdmin:
		return true
	default:
		return false
	}
}

func (p Principal) CanAdmin() bool {
	return p.Role == RoleAdmin
}

func RequireRead(ctx context.Context) error {
	return requirePermission(ctx, func(p Principal) bool { return p.CanRead() }, "read")
}

func RequireWrite(ctx context.Context) error {
	return requirePermission(ctx, func(p Principal) bool { return p.CanWrite() }, "write")
}

func RequireAdmin(ctx context.Context) error {
	return requirePermission(ctx, func(p Principal) bool { return p.CanAdmin() }, "admin")
}

func requirePermission(ctx context.Context, allowed func(Principal) bool, action string) error {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil
	}
	if allowed(p) {
		return nil
	}
	return errors.Join(ErrForbidden, errors.New(action+" permission required"))
}

func ApplyPrincipalActor(ctx context.Context, actor domain.Actor) (domain.Actor, error) {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return actor, nil
	}
	if actorIsEmpty(actor) {
		return p.Actor, nil
	}
	if actorsMatch(actor, p.Actor) {
		return actor, nil
	}
	return domain.Actor{}, errors.Join(ErrForbidden, errors.New("actor must match authenticated identity"))
}

func actorIsEmpty(actor domain.Actor) bool {
	return strings.TrimSpace(string(actor.Kind)) == "" && strings.TrimSpace(actor.Name) == ""
}

func actorsMatch(a, b domain.Actor) bool {
	ak, err := domain.ParseActorKind(string(a.Kind))
	if err != nil {
		return false
	}
	bk, err := domain.ParseActorKind(string(b.Kind))
	if err != nil {
		return false
	}
	return ak == bk && strings.TrimSpace(a.Name) == strings.TrimSpace(b.Name)
}
