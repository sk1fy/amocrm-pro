package corelifecycle

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

const snapshotAudience = "platform_installations"
const snapshotAction = "snapshot"
const snapshotTTL = 30 * time.Second

type SnapshotService struct {
	store *Store
	key   ed25519.PrivateKey
}

type snapshotClaims struct {
	jwt.RegisteredClaims
	Integration uuid.UUID `json:"integration_id"`
	Action      string    `json:"action"`
}

func NewSnapshotService(store *Store, keyPath string) (*SnapshotService, error) {
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, errors.New("could not read Core snapshot delegation key")
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("invalid Core snapshot delegation key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(ed25519.PrivateKey)
	if err != nil || !ok {
		return nil, errors.New("Core snapshot delegation key must be Ed25519")
	}
	return &SnapshotService{store: store, key: key}, nil
}

func (s *SnapshotService) Issue(ctx context.Context, installation, integration uuid.UUID) (string, error) {
	if installation == uuid.Nil || integration == uuid.Nil {
		return "", serviceapi.Fail(serviceapi.InvalidArgument, "invalid snapshot scope")
	}
	if _, err := s.snapshot(ctx, installation, integration); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	claims := snapshotClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "core", Subject: installation.String(), Audience: jwt.ClaimStrings{snapshotAudience},
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(snapshotTTL)), ID: uuid.NewString(),
		},
		Integration: integration, Action: snapshotAction,
	}
	return jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(s.key)
}

func (s *SnapshotService) GetSnapshot(ctx context.Context, auth string, installation uuid.UUID) (serviceapi.CoreInstallationSnapshot, error) {
	if installation == uuid.Nil {
		return serviceapi.CoreInstallationSnapshot{}, serviceapi.Fail(serviceapi.InvalidArgument, "invalid installation")
	}
	var claims snapshotClaims
	if len(auth) == 0 || len(auth) > 4096 {
		return serviceapi.CoreInstallationSnapshot{}, serviceapi.Fail(serviceapi.Unauthenticated, "invalid snapshot delegation")
	}
	token, err := jwt.ParseWithClaims(auth, &claims, func(*jwt.Token) (any, error) { return s.key.Public(), nil },
		jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer("core"), jwt.WithAudience(snapshotAudience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithNotBeforeRequired())
	if err != nil || !token.Valid || claims.IssuedAt == nil || len(claims.Audience) != 1 ||
		claims.ExpiresAt.Sub(claims.IssuedAt.Time) <= 0 || claims.ExpiresAt.Sub(claims.IssuedAt.Time) > snapshotTTL {
		return serviceapi.CoreInstallationSnapshot{}, serviceapi.Fail(serviceapi.Unauthenticated, "invalid snapshot delegation")
	}
	if claims.Subject != installation.String() || claims.Integration == uuid.Nil || claims.Action != snapshotAction {
		return serviceapi.CoreInstallationSnapshot{}, serviceapi.Fail(serviceapi.PermissionDenied, "snapshot delegation scope denied")
	}
	return s.snapshot(ctx, installation, claims.Integration)
}

func (s *SnapshotService) snapshot(ctx context.Context, installation, integration uuid.UUID) (serviceapi.CoreInstallationSnapshot, error) {
	result := serviceapi.CoreInstallationSnapshot{InstallationID: installation, IntegrationID: integration}
	var installationStatus, integrationStatus string
	var hasCredentials bool
	err := s.store.pool.QueryRow(ctx, `
		SELECT i.account_id,NULLIF(i.account_domain,''),i.status,n.status,
			EXISTS(SELECT 1 FROM oauth_credentials o WHERE o.installation_id=i.id),
			statement_timestamp(), c.generation,c.sequence,c.authorization_sequence
		FROM installations i JOIN integrations n ON n.id=i.integration_id
		JOIN platform_lifecycle_scope p ON p.integration_id=i.integration_id AND p.account_id=i.account_id
		LEFT JOIN platform_lifecycle_cursors c ON c.installation_id=i.id
		WHERE i.id=$1 AND i.integration_id=$2`, installation, integration).Scan(&result.AccountID, &result.Domain,
		&installationStatus, &integrationStatus, &hasCredentials, &result.ObservedAt,
		&result.Generation, &result.Sequence, &result.AuthorizationSequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return serviceapi.CoreInstallationSnapshot{}, serviceapi.Fail(serviceapi.PermissionDenied, "snapshot scope denied")
	}
	if err != nil {
		return serviceapi.CoreInstallationSnapshot{}, serviceapi.Fail(serviceapi.Unavailable, "Core installation snapshot unavailable")
	}
	result.Authorization = EffectiveAuthorization(integrationStatus, installationStatus)
	if result.Authorization == wire.Authorized && !hasCredentials {
		result.Authorization = wire.Unknown
	}
	return result, nil
}
