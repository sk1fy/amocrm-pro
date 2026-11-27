package corelifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

func (s *Store) AccountLicenseScope(ctx context.Context, principal widgetauth.Principal, receivedAt time.Time) (wire.ActivationScope, error) {
	scope, err := s.AccessProofScope(ctx, principal)
	if err != nil {
		return wire.ActivationScope{}, err
	}
	var body []byte
	err = s.pool.QueryRow(ctx, `
		SELECT l.body FROM platform_lifecycle_cursors c
		JOIN platform_lifecycle_ledger l ON l.installation_id=c.installation_id AND l.sequence=c.authorization_sequence
		WHERE c.installation_id=$1 AND c.generation=$2 AND c.authorization_sequence=$3 AND NOT c.closed`,
		scope.Installation, scope.Generation, scope.AuthorizationSequence).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return wire.ActivationScope{}, ErrAccessProofDenied
	}
	if err != nil {
		return wire.ActivationScope{}, err
	}
	authorization, err := wire.DecodeEvent(body)
	if err != nil {
		return wire.ActivationScope{}, err
	}
	// The precise receipt fences requests that arrived before this authorization.
	// After reauthorization the issuer may be ahead of Core, and JWT issue times
	// have only whole-second precision. Ask for a fresh report in that window.
	issuedAt := principal.TokenIssuedAt
	authorizedSecond := authorization.OccurredAt.Truncate(time.Second)
	if issuedAt.Before(authorizedSecond) || receivedAt.Before(authorization.OccurredAt) {
		return wire.ActivationScope{}, ErrAccessProofDenied
	}
	if scope.AuthorizationSequence > 1 && !issuedAt.After(authorization.OccurredAt.Add(principal.TokenClockLeeway).Truncate(time.Second)) {
		return wire.ActivationScope{}, errors.New("widget token issue time overlaps the current authorization change")
	}
	return wire.ActivationScope{
		Installation: scope.Installation, Integration: scope.Integration, Account: principal.AccountID,
		Generation: scope.Generation, AuthorizationSequence: scope.AuthorizationSequence,
	}, nil
}

type AccountLicenseClient struct {
	endpoint string
	client   *http.Client
}

func NewAccountLicenseClient(origin, identityDir string) (*AccountLicenseClient, error) {
	endpoint, client, err := platformClient(origin, identityDir, wire.AccountLicensePath)
	if err != nil {
		return nil, err
	}
	return &AccountLicenseClient{endpoint: endpoint.String(), client: client}, nil
}

func (c *AccountLicenseClient) Close() { c.client.CloseIdleConnections() }

func (c *AccountLicenseClient) Report(ctx context.Context, input wire.AccountLicenseRequest) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return errors.New("Platform account license unavailable")
	}
	return nil
}
