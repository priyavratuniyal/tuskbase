package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/priyavratuniyal/tuskbase/internal/app"
	"github.com/priyavratuniyal/tuskbase/internal/domain"
)

const (
	localAPIKeyScope = "tuskbase"
	roleReader       = string(app.RoleReader)
	roleAgent        = string(app.RoleAgent)
	roleAdmin        = string(app.RoleAdmin)
)

// LocalAPIKeyPolicy protects HTTP transports with a single local bearer token.
// Local Basic intentionally keeps this coarse identity; Local Shared should use
// named local keys when per-client attribution matters.
type LocalAPIKeyPolicy struct {
	key    string
	source string
}

func NewLocalAPIKeyPolicy(key string) (LocalAPIKeyPolicy, error) {
	return NewLocalAPIKeyPolicyWithSource(key, "manual")
}

func NewLocalAPIKeyPolicyWithSource(key, source string) (LocalAPIKeyPolicy, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return LocalAPIKeyPolicy{}, errors.New("local API key is required")
	}
	return LocalAPIKeyPolicy{key: key, source: cleanSource(source)}, nil
}

func (p LocalAPIKeyPolicy) WrapHTTP(h http.Handler) http.Handler {
	verifier := func(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error) {
		if !sameSecret(token, p.key) {
			return nil, auth.ErrInvalidToken
		}
		return tokenInfo(localAPIKeyPrincipal()), nil
	}
	return requireTuskbaseBearer(verifier)(h)
}

func (LocalAPIKeyPolicy) Name() string     { return "local-api-key" }
func (p LocalAPIKeyPolicy) Source() string { return p.source }

// LocalSharedKey describes one named local client credential.
type LocalSharedKey struct {
	Name string
	Role string
	Key  string
}

// LocalSharedKeyPolicy protects HTTP transports with per-agent local bearer tokens.
// This is enough for on-machine Local Shared usage without adding OAuth ceremony.
type LocalSharedKeyPolicy struct {
	keys   []LocalSharedKey
	source string
}

func NewLocalSharedKeyPolicy(keys []LocalSharedKey) (LocalSharedKeyPolicy, error) {
	return NewLocalSharedKeyPolicyWithSource(keys, "manual")
}

func NewLocalSharedKeyPolicyWithSource(keys []LocalSharedKey, source string) (LocalSharedKeyPolicy, error) {
	if len(keys) == 0 {
		return LocalSharedKeyPolicy{}, errors.New("at least one local shared key is required")
	}
	seen := map[string]struct{}{}
	cleaned := make([]LocalSharedKey, 0, len(keys))
	for _, key := range keys {
		key.Name = strings.TrimSpace(key.Name)
		role, err := NormalizeLocalRole(key.Role)
		if err != nil {
			return LocalSharedKeyPolicy{}, err
		}
		key.Role = role
		key.Key = strings.TrimSpace(key.Key)
		if key.Name == "" {
			return LocalSharedKeyPolicy{}, errors.New("local shared key name is required")
		}
		if strings.ContainsAny(key.Name, ",:\t\n\r ") {
			return LocalSharedKeyPolicy{}, fmt.Errorf("local shared key name %q cannot contain whitespace, comma, or colon", key.Name)
		}
		if key.Key == "" {
			return LocalSharedKeyPolicy{}, fmt.Errorf("local shared key for %q is required", key.Name)
		}
		if _, ok := seen[key.Name]; ok {
			return LocalSharedKeyPolicy{}, fmt.Errorf("duplicate local shared key name %q", key.Name)
		}
		seen[key.Name] = struct{}{}
		cleaned = append(cleaned, key)
	}
	return LocalSharedKeyPolicy{keys: cleaned, source: cleanSource(source)}, nil
}

// ParseLocalSharedKeys parses comma-separated name:role:key specs.
// Example: codex:agent:secret,claude:reader:other-secret
func ParseLocalSharedKeys(raw string) ([]LocalSharedKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	keys := make([]LocalSharedKey, 0, len(parts))
	for _, part := range parts {
		fields := strings.SplitN(strings.TrimSpace(part), ":", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("local shared key %q must use name:role:key", part)
		}
		keys = append(keys, LocalSharedKey{Name: fields[0], Role: fields[1], Key: fields[2]})
	}
	return keys, nil
}

func (p LocalSharedKeyPolicy) WrapHTTP(h http.Handler) http.Handler {
	verifier := func(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error) {
		for _, key := range p.keys {
			if sameSecret(token, key.Key) {
				return tokenInfo(localSharedKeyPrincipal(key)), nil
			}
		}
		return nil, auth.ErrInvalidToken
	}
	return requireTuskbaseBearer(verifier)(h)
}

func (LocalSharedKeyPolicy) Name() string     { return "local-shared-keys" }
func (p LocalSharedKeyPolicy) Source() string { return p.source }

func requireTuskbaseBearer(verifier auth.TokenVerifier) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		withPrincipal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ti := auth.TokenInfoFromContext(r.Context()); ti != nil {
				if principal, ok := principalFromTokenInfo(ti); ok {
					r = r.WithContext(app.ContextWithPrincipal(r.Context(), principal))
				}
			}
			h.ServeHTTP(w, r)
		})
		return auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{Scopes: []string{localAPIKeyScope}})(withPrincipal)
	}
}

func tokenInfo(principal app.Principal) *auth.TokenInfo {
	return &auth.TokenInfo{
		Scopes:     roleScopes(string(principal.Role)),
		Expiration: time.Now().Add(time.Hour),
		UserID:     principal.Subject,
		Extra:      map[string]any{"role": string(principal.Role), "principal": principal},
	}
}

func principalFromTokenInfo(ti *auth.TokenInfo) (app.Principal, bool) {
	principal, ok := ti.Extra["principal"].(app.Principal)
	return principal, ok
}

func localAPIKeyPrincipal() app.Principal {
	return app.Principal{
		Subject:    "local-api-key",
		Role:       app.RoleAgent,
		Actor:      domain.Actor{Kind: domain.ActorAgent, Name: "local-api-key"},
		AuthSource: app.AuthSourceLocalAPIKey,
	}
}

func localSharedKeyPrincipal(key LocalSharedKey) app.Principal {
	return app.Principal{
		Subject:    strings.TrimSpace(key.Name),
		Role:       app.Role(key.Role),
		Actor:      domain.Actor{Kind: domain.ActorAgent, Name: strings.TrimSpace(key.Name)},
		AuthSource: app.AuthSourceLocalSharedKey,
	}
}

func roleScopes(role string) []string {
	switch role {
	case roleReader:
		return []string{localAPIKeyScope, "tuskbase:read"}
	case roleAgent:
		return []string{localAPIKeyScope, "tuskbase:read", "tuskbase:write"}
	case roleAdmin:
		return []string{localAPIKeyScope, "tuskbase:read", "tuskbase:write", "tuskbase:admin"}
	default:
		return []string{localAPIKeyScope}
	}
}

func NormalizeLocalRole(role string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case roleReader:
		return roleReader, nil
	case "", roleAgent:
		return roleAgent, nil
	case roleAdmin:
		return roleAdmin, nil
	default:
		return "", fmt.Errorf("unsupported local shared role %q", role)
	}
}

func cleanSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "unknown"
	}
	return source
}

func sameSecret(got, want string) bool {
	gotHash := sha256.Sum256([]byte(got))
	wantHash := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
}
