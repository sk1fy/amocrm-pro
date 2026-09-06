package amocrm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
	"net/http"
	"strings"
	"time"
)

// BootstrapAccount reads the fixed account endpoint using a newly exchanged
// OAuth token, before credentials can be persisted. This reuses the one client's
// HTTP transport, integration limiter, canonical account limiter and metrics.
// Known account IDs come from Core's domain lookup across every integration.
func (c *Client) BootstrapAccount(ctx context.Context, integrationID uuid.UUID, knownAccountID int64, domain, token string) (Account, error) {
	if integrationID == uuid.Nil || knownAccountID < 0 || len(token) == 0 || len(token) > 16384 || strings.ContainsAny(token, "\r\n") {
		return Account{}, errors.New("invalid bootstrap account request")
	}
	base, err := AccountBaseURL(domain)
	if err != nil {
		return Account{}, err
	}
	domain = base.Host
	c.bootstrapMu.Lock()
	if knownAccountID == 0 {
		knownAccountID = c.bootstrapAccounts[domain]
	}
	if knownAccountID == 0 && len(c.bootstrapAccounts) >= 4096 {
		c.bootstrapMu.Unlock()
		return Account{}, errors.New("bootstrap account identity capacity exhausted")
	}
	c.bootstrapMu.Unlock()
	access := AccessToken{IntegrationID: integrationID, AccountID: knownAccountID, AccountDomain: domain, Value: token}
	// Account0 is a single bounded discovery bucket, never one bucket per token.
	if err := c.waitBudget(ctx, access); err != nil {
		return Account{}, fmt.Errorf("wait for bootstrap budget: %w", err)
	}
	status, header, body, err := c.request(ctx, access, http.MethodGet, "/api/v4/account", nil)
	if err != nil {
		return Account{}, err
	}
	if status < 200 || status >= 300 {
		return Account{}, classifyResponse(status, header, time.Now())
	}
	var account Account
	if err := json.Unmarshal(body, &account); err != nil {
		return Account{}, ErrIncompleteResponse
	}
	if account.ID <= 0 || !accountLabel.MatchString(account.Subdomain) || (knownAccountID > 0 && knownAccountID != account.ID) {
		return Account{}, ErrIncompleteResponse
	}
	if knownAccountID == 0 {
		c.bootstrapMu.Lock()
		if c.bootstrapAccounts == nil {
			c.bootstrapAccounts = map[string]int64{}
		}
		c.bootstrapAccounts[domain] = account.ID
		c.bootstrapMu.Unlock()
		// The request already happened. Debit its newly identified canonical account
		// before exposing the ID to SaveInstallation; cancellation must not refund it.
		start := time.Now()
		err := c.limiter.chargeAccount(ctx, account.ID)
		if c.metrics != nil {
			outcome := "admitted"
			if err != nil {
				outcome = "canceled"
			}
			c.metrics.wait.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
		}
		if err != nil {
			return Account{}, fmt.Errorf("debit discovered account budget: %w", err)
		}
	}
	return account, nil
}
func (l *limiter) chargeAccount(ctx context.Context, accountID int64) error {
	l.mu.Lock()
	bucket := l.accounts[accountID]
	if bucket == nil {
		bucket = rate.NewLimiter(l.accountRate, l.accountBurst)
		l.accounts[accountID] = bucket
	}
	l.mu.Unlock()
	now := time.Now()
	reservation := bucket.ReserveN(now, 1)
	if !reservation.OK() {
		return errors.New("account budget cannot admit bootstrap debit")
	}
	delay := reservation.DelayFrom(now)
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
