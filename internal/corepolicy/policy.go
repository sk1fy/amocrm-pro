// Package corepolicy owns Core's live pilot policy and short-lived delegation.
// It is the only new component with access to the Core DB and signing key.
package corepolicy

import (
	"context"
	"crypto/ed25519"
	"errors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"time"
)

const TokenLifetime = 30 * time.Second
const ValidationTimeout = 10 * time.Second

type callerKey struct{}

// WithCaller is a composition/transport hook. Local adapters set this identity;
// remote adapters derive it exclusively from verified mTLS certificates.
func WithCaller(ctx context.Context, caller string) context.Context {
	return context.WithValue(ctx, callerKey{}, caller)
}
func Caller(ctx context.Context) string { s, _ := ctx.Value(callerKey{}).(string); return s }

type fixedCaller struct {
	policy serviceapi.Policy
	caller string
}

func ForCaller(p serviceapi.Policy, caller string) serviceapi.Policy { return fixedCaller{p, caller} }
func (f fixedCaller) Issue(ctx context.Context, r serviceapi.IssueRequest) (serviceapi.Auth, error) {
	return f.policy.Issue(WithCaller(ctx, f.caller), r)
}
func (f fixedCaller) Validate(ctx context.Context, a serviceapi.Auth, s, v string) (serviceapi.Principal, error) {
	return f.policy.Validate(WithCaller(ctx, f.caller), a, s, v)
}

type LiveChecker interface {
	Check(context.Context, serviceapi.Scope, int64, bool) error
}

// DelegationChecker rechecks local revocation without repeating the upstream
// role lookup already certified by Issue. Implementations must check current
// installation, capability, pilot and reauthorization state on every call.
// Check remains mandatory for every newly issued delegation. Legacy checkers
// without this interface keep their more conservative full Check behavior.
type DelegationChecker interface {
	CheckDelegation(context.Context, serviceapi.Scope) error
}
type Service struct {
	checker LiveChecker
	key     ed25519.PrivateKey
	now     func() time.Time
}

func New(pool *pgxpool.Pool, client *amocrm.Client, key ed25519.PrivateKey) (*Service, error) {
	return NewWithChecker(&databaseChecker{pool, client}, key)
}
func NewWithChecker(checker LiveChecker, key ed25519.PrivateKey) (*Service, error) {
	if checker == nil || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("policy requires a live checker and Ed25519 private key")
	}
	return &Service{checker: checker, key: append(ed25519.PrivateKey(nil), key...), now: time.Now}, nil
}

type claims struct {
	jwt.RegisteredClaims
	Scope     serviceapi.Scope   `json:"scope"`
	ActorID   int64              `json:"actor_id"`
	System    bool               `json:"system"`
	Consumer  string             `json:"consumer"`
	RequestID string             `json:"request_id"`
	Grants    []serviceapi.Grant `json:"grants"`
}

func (s *Service) Issue(ctx context.Context, r serviceapi.IssueRequest) (serviceapi.Auth, error) {
	caller := Caller(ctx)
	if caller != serviceapi.CoreService && caller != serviceapi.EventsService {
		return serviceapi.Auth{}, serviceapi.Fail(serviceapi.PermissionDenied, "issuer identity is not authorized")
	}
	if r.IntegrationID == uuid.Nil || r.InstallationID == uuid.Nil || len(r.RequestID) == 0 || len(r.RequestID) > 128 || r.Consumer != serviceapi.ActivityService || len(r.Grants) == 0 || len(r.Grants) > 12 || (!r.System && r.ActorID <= 0) || (r.System && r.ActorID != 0) {
		return serviceapi.Auth{}, serviceapi.Fail(serviceapi.InvalidArgument, "invalid delegation request")
	}
	if caller == serviceapi.EventsService && !r.System {
		return serviceapi.Auth{}, serviceapi.Fail(serviceapi.PermissionDenied, "collector cannot delegate a user")
	}
	if caller == serviceapi.CoreService && r.System {
		return serviceapi.Auth{}, serviceapi.Fail(serviceapi.PermissionDenied, "Core ingress must preserve the original actor")
	}
	for _, g := range r.Grants {
		if !allowedGrant(g, r.System) {
			return serviceapi.Auth{}, serviceapi.Fail(serviceapi.PermissionDenied, "delegation grant is not authorized")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, ValidationTimeout)
	defer cancel()
	if err := s.checker.Check(ctx, r.Scope, r.ActorID, r.System); err != nil {
		return serviceapi.Auth{}, err
	}
	now := s.now()
	c := claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "amocrm-pro-core", Audience: jwt.ClaimStrings{"amocrm-pro-services-v1"}, IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(TokenLifetime)), ID: uuid.NewString()}, Scope: r.Scope, ActorID: r.ActorID, System: r.System, Consumer: r.Consumer, RequestID: r.RequestID, Grants: r.Grants}
	token, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c).SignedString(s.key)
	if err != nil {
		return serviceapi.Auth{}, serviceapi.Fail(serviceapi.Internal, "sign delegation")
	}
	return serviceapi.Auth{Token: token}, nil
}
func allowedGrant(g serviceapi.Grant, system bool) bool {
	if system {
		return (g.Audience == serviceapi.GatewayService && g.Action == serviceapi.ActionEvents) || (g.Audience == serviceapi.EventsService && (g.Action == serviceapi.ActionSync || g.Action == serviceapi.ActionStatus))
	}
	switch g.Audience {
	case serviceapi.ActivityService:
		return g.Action == serviceapi.ActionPanel || g.Action == serviceapi.ActionSettings || g.Action == serviceapi.ActionOperation
	case serviceapi.EventsService:
		return g.Action == serviceapi.ActionRead || g.Action == serviceapi.ActionStatus || g.Action == serviceapi.ActionSync || g.Action == serviceapi.ActionOperation
	case serviceapi.GatewayService:
		return g.Action == serviceapi.ActionUsers
	}
	return false
}
func (s *Service) Validate(ctx context.Context, a serviceapi.Auth, audience, action string) (serviceapi.Principal, error) {
	if Caller(ctx) != audience && Caller(ctx) != serviceapi.CoreService {
		return serviceapi.Principal{}, serviceapi.Fail(serviceapi.PermissionDenied, "policy caller does not own this audience")
	}
	if len(a.Token) == 0 || len(a.Token) > 8192 {
		return serviceapi.Principal{}, serviceapi.Fail(serviceapi.Unauthenticated, "invalid delegation")
	}
	c := new(claims)
	_, err := jwt.ParseWithClaims(a.Token, c, func(t *jwt.Token) (any, error) { return s.key.Public(), nil }, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer("amocrm-pro-core"), jwt.WithAudience("amocrm-pro-services-v1"), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(s.now))
	if err != nil || c.ExpiresAt == nil || c.IssuedAt == nil || c.ExpiresAt.Time.Sub(c.IssuedAt.Time) > TokenLifetime || c.Scope.InstallationID == uuid.Nil || c.Scope.IntegrationID == uuid.Nil || c.Consumer != serviceapi.ActivityService {
		return serviceapi.Principal{}, serviceapi.Fail(serviceapi.Unauthenticated, "invalid or expired delegation")
	}
	ok := false
	for _, g := range c.Grants {
		if g.Audience == audience && g.Action == action && allowedGrant(g, c.System) {
			ok = true
		}
	}
	if !ok {
		return serviceapi.Principal{}, serviceapi.Fail(serviceapi.PermissionDenied, "delegation audience or action denied")
	}
	ctx, cancel := context.WithTimeout(ctx, ValidationTimeout)
	defer cancel()
	var policyErr error
	if checker, ok := s.checker.(DelegationChecker); ok {
		policyErr = checker.CheckDelegation(ctx, c.Scope)
	} else {
		policyErr = s.checker.Check(ctx, c.Scope, c.ActorID, c.System)
	}
	if policyErr != nil {
		return serviceapi.Principal{}, policyErr
	}
	return serviceapi.Principal{Scope: c.Scope, ActorID: c.ActorID, System: c.System, Consumer: c.Consumer, RequestID: c.RequestID, ExpiresAt: c.ExpiresAt.Time}, nil
}

type authorizationReader interface {
	GetUserAuthorization(context.Context, uuid.UUID, int64) (amocrm.UserAuthorization, error)
}
type databaseChecker struct {
	pool   *pgxpool.Pool
	client authorizationReader
}

func (d *databaseChecker) Check(ctx context.Context, scope serviceapi.Scope, actor int64, system bool) error {
	if err := d.CheckDelegation(ctx, scope); err != nil {
		return err
	}
	if system {
		return nil
	}
	user, err := d.client.GetUserAuthorization(ctx, scope.InstallationID, actor)
	if err != nil {
		return MapUpstreamError(err)
	}
	if !user.Rights.IsActive || !user.Rights.IsAdmin {
		return serviceapi.Fail(serviceapi.PermissionDenied, "active administrator role is required")
	}
	return nil
}

func (d *databaseChecker) CheckDelegation(ctx context.Context, scope serviceapi.Scope) error {
	var enabled bool
	var reauth bool
	err := d.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM installations i JOIN integrations n ON n.id=i.integration_id JOIN integration_services c ON c.integration_id=n.id AND c.service_code='activity' JOIN activity_pilots p ON p.installation_id=i.id WHERE i.id=$1 AND n.id=$2 AND i.status='active' AND n.status='active' AND c.enabled AND p.enabled), EXISTS(SELECT 1 FROM installations WHERE id=$1 AND integration_id=$2 AND status='reauth_required')`, scope.InstallationID, scope.IntegrationID).Scan(&enabled, &reauth)
	if err != nil {
		return serviceapi.Fail(serviceapi.Unavailable, "live policy is unavailable")
	}
	if reauth {
		return serviceapi.Fail(serviceapi.ReauthRequired, "installation requires authorization")
	}
	if !enabled {
		return serviceapi.Fail(serviceapi.PermissionDenied, "Activity pilot or capability is disabled")
	}
	return nil
}

// MapUpstreamError exposes only finite categories, never token or HTTP payloads.
func MapUpstreamError(err error) error {
	var api *amocrm.APIError
	if errors.As(err, &api) {
		switch api.Kind {
		case amocrm.ErrorUnauthorized:
			return serviceapi.Fail(serviceapi.ReauthRequired, "installation requires authorization")
		case amocrm.ErrorForbidden, amocrm.ErrorPayment:
			return serviceapi.Fail(serviceapi.PermissionDenied, "amoCRM access denied")
		case amocrm.ErrorValidation:
			return serviceapi.Fail(serviceapi.InvalidArgument, "amoCRM rejected request")
		case amocrm.ErrorNotFound:
			return serviceapi.Fail(serviceapi.NotFound, "amoCRM resource not found")
		case amocrm.ErrorRateLimited:
			return &serviceapi.Error{Code: serviceapi.ResourceExhausted, Message: "amoCRM request budget exhausted", RetryAfter: api.RetryAfter}
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return serviceapi.Fail(serviceapi.DeadlineExceeded, "upstream deadline exceeded")
	}
	return serviceapi.Fail(serviceapi.Unavailable, "amoCRM is unavailable")
}
