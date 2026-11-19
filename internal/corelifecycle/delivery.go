package corelifecycle

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

func platformClient(origin, identityDir, path string) (*url.URL, *http.Client, error) {
	endpoint, err := url.Parse(origin)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil ||
		endpoint.ForceQuery || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return nil, nil, errors.New("Platform delivery requires a private HTTPS origin")
	}
	endpoint.Path = path
	ca, err := os.ReadFile(filepath.Join(identityDir, "ca.crt"))
	if err != nil {
		return nil, nil, errors.New("could not read Platform delivery CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, nil, errors.New("invalid Platform delivery CA")
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(identityDir, "tls.crt"), filepath.Join(identityDir, "tls.key"))
	if err != nil {
		return nil, nil, errors.New("could not load Core delivery identity")
	}
	client := &http.Client{
		Timeout:       8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{MaxIdleConnsPerHost: 8, IdleConnTimeout: time.Minute,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, RootCAs: roots,
				VerifyConnection: func(state tls.ConnectionState) error {
					if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 ||
						len(state.PeerCertificates[0].URIs) != 1 || state.PeerCertificates[0].URIs[0].String() != wire.PlatformIdentity {
						return errors.New("Platform delivery peer identity mismatch")
					}
					return nil
				},
			}},
	}
	return endpoint, client, nil
}

func DeliveryHandler(store *Store, origin, identityDir string) (jobs.Handler, error) {
	endpoint, client, err := platformClient(origin, identityDir, wire.EventsPath)
	if err != nil {
		return nil, err
	}

	return func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
		var payload struct {
			EventID uuid.UUID `json:"event_id"`
		}
		if job.InstallationID == nil || job.ResourceID == nil || job.ResourceType == nil ||
			*job.ResourceType != "platform_lifecycle" || json.Unmarshal(job.Payload, &payload) != nil ||
			payload.EventID == uuid.Nil || *job.ResourceID != payload.EventID.String() {
			return nil, jobs.Permanent("invalid_lifecycle_delivery", errors.New("invalid Platform delivery job scope"))
		}
		var raw []byte
		err := store.pool.QueryRow(ctx, `
			SELECT body FROM platform_lifecycle_ledger WHERE event_id=$1 AND installation_id=$2`, payload.EventID, job.InstallationID).Scan(&raw)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			return nil, errors.New("Platform lifecycle delivery unavailable")
		}
		defer response.Body.Close()
		if err := deliveryResponseError(response, time.Now()); err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, wire.MaxBody+1))
		var receipt wire.Receipt
		if err != nil || wire.Decode(body, &receipt) != nil || receipt.EventID != payload.EventID || receipt.State != "recorded" {
			return nil, errors.New("invalid Platform lifecycle receipt")
		}
		if _, err := store.pool.Exec(ctx, `
			INSERT INTO platform_lifecycle_deliveries(event_id) VALUES($1) ON CONFLICT DO NOTHING`,
			payload.EventID); err != nil {
			return nil, err
		}
		return json.Marshal(receipt)
	}, nil
}

func deliveryResponseError(response *http.Response, now time.Time) error {
	switch response.StatusCode {
	case http.StatusAccepted:
		return nil
	case http.StatusConflict:
		return jobs.Permanent("platform_lifecycle_conflict", errors.New("Platform lifecycle conflict requires reconciliation"))
	case http.StatusBadRequest, http.StatusForbidden, http.StatusUnauthorized, http.StatusNotFound:
		return jobs.Permanent("platform_lifecycle_rejected", errors.New("Platform rejected lifecycle delivery"))
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return jobs.Retryable("platform_lifecycle_unavailable", retryAfter(response.Header.Get("Retry-After"), now),
			errors.New("Platform lifecycle receipt unavailable"))
	default:
		return errors.New("Platform lifecycle receipt unavailable")
	}
}

func retryAfter(raw string, now time.Time) time.Duration {
	const maxDelay = 10 * time.Minute
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if strings.Trim(raw, "0123456789") == "" {
		seconds, err := strconv.ParseUint(raw, 10, 64)
		if errors.Is(err, strconv.ErrRange) || seconds > uint64(maxDelay/time.Second) {
			return maxDelay
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(raw); err == nil && date.After(now) {
		return min(date.Sub(now), maxDelay)
	}
	return 0
}

func (s *Store) Replay(ctx context.Context, installation uuid.UUID, first, last int64) error {
	if installation == uuid.Nil || first < 1 || last < first || last > wire.MaxPosition || last-first >= 100 {
		return errors.New("replay requires one installation and 1 to 100 consecutive positions")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT l.event_id FROM platform_lifecycle_ledger l
		JOIN installations i ON i.id=l.installation_id
		JOIN platform_lifecycle_scope s ON s.integration_id=i.integration_id AND s.account_id=i.account_id
		WHERE l.installation_id=$1 AND l.sequence BETWEEN $2 AND $3 ORDER BY l.sequence`, installation, first, last)
	if err != nil {
		return err
	}
	var events []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		events = append(events, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if int64(len(events)) != last-first+1 {
		return errors.New("replay range is absent from the scoped Core ledger")
	}
	for _, event := range events {
		if err := s.enqueue(ctx, tx, installation, event); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
