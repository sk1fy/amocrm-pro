package companysource

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/companycrm"
)

const MaxBody = 8 << 20

type Config struct {
	Origin, CAFile, CertFile, KeyFile string
	AccountID                         int64
	Revision                          string
}

func ParseConfig(getenv func(string) string) (Config, error) {
	c := Config{Origin: getenv("PLATFORM_COMPANY_CORE_ORIGIN"), CAFile: getenv("PLATFORM_COMPANY_CORE_CA_FILE"), CertFile: getenv("PLATFORM_COMPANY_CORE_CERT_FILE"), KeyFile: getenv("PLATFORM_COMPANY_CORE_KEY_FILE"), Revision: getenv("PLATFORM_COMPANY_BINDING_REVISION")}
	account := getenv("PLATFORM_COMPANY_SOURCE_ACCOUNT_ID")
	if c.Origin == "" && c.CAFile == "" && c.CertFile == "" && c.KeyFile == "" && c.Revision == "" && account == "" {
		return c, nil
	}
	var err error
	c.AccountID, err = strconv.ParseInt(account, 10, 64)
	if err != nil || strconv.FormatInt(c.AccountID, 10) != account {
		return Config{}, errors.New("invalid company source account")
	}
	return c, c.validate()
}
func (c Config) validate() error {
	u, err := url.Parse(c.Origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.String() != c.Origin || c.CAFile == "" || c.CertFile == "" || c.KeyFile == "" || c.AccountID <= 0 || len(c.Revision) == 0 || len(c.Revision) > 256 {
		return errors.New("company source requires fixed HTTPS origin, TLS files, account and binding revision")
	}
	return nil
}

type unavailableError struct{ cause error }

func (e unavailableError) Error() string { return "company source unavailable" }
func (e unavailableError) Unwrap() error { return e.cause }

type Client struct {
	config Config
	http   *http.Client
}
type Result[T any] struct {
	Response companycrm.Response[T]
	Status   int
	RetryAt  *time.Time
}

func New(c Config) (*Client, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, errors.New("load company source TLS identity")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || len(leaf.URIs) != 1 || leaf.URIs[0].String() != "spiffe://amocrm-pro/platform-company-crm" {
		return nil, errors.New("incorrect company source client role")
	}
	pem, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, errors.New("read company source CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("invalid company source CA")
	}
	t := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair}, VerifyConnection: func(s tls.ConnectionState) error {
		if len(s.VerifiedChains) == 0 || len(s.PeerCertificates) == 0 || len(s.PeerCertificates[0].URIs) != 1 || s.PeerCertificates[0].URIs[0].String() != "spiffe://amocrm-pro/core" {
			return errors.New("incorrect Core company source identity")
		}
		return nil
	}}
	transport := &http.Transport{TLSClientConfig: t, Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 16 << 10, MaxIdleConnsPerHost: 2, IdleConnTimeout: time.Minute}
	return &Client{config: c, http: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("company source redirect rejected") }}}, nil
}
func (c *Client) Close()                 { c.http.CloseIdleConnections() }
func (c *Client) Scope() (int64, string) { return c.config.AccountID, c.config.Revision }
func (c *Client) Clients(ctx context.Context) (Result[companycrm.Clients], error) {
	result, err := read[companycrm.Clients](ctx, c, "/v1/company-crm/clients")
	if err != nil || !result.Response.Source.Complete {
		return result, err
	}
	data := result.Response.Data
	valid := data != nil && data.Companies != nil && data.Contacts != nil
	if valid {
		for _, company := range data.Companies {
			if company.AccountIDs == nil || company.Resolution != "zero" && company.Resolution != "one" && company.Resolution != "multiple" && company.Resolution != "unknown" {
				valid = false
				break
			}
		}
		for _, contact := range data.Contacts {
			if contact.CompanyIDs == nil {
				valid = false
				break
			}
		}
	}
	if !valid {
		result.Response.Data = nil
		return result, errors.New("invalid company source client data")
	}
	return result, nil
}
func (c *Client) WorkflowMetadata(ctx context.Context) (Result[companycrm.WorkflowMetadata], error) {
	result, err := read[companycrm.WorkflowMetadata](ctx, c, "/v1/company-crm/workflow-metadata")
	if err != nil || !result.Response.Source.Complete {
		return result, err
	}
	data := result.Response.Data
	valid := data != nil && data.Pipelines != nil && data.LeadFields != nil && data.RequiredFieldsState == "unknown"
	if valid {
		for _, pipeline := range data.Pipelines {
			if pipeline.Embedded.Statuses == nil {
				valid = false
				break
			}
		}
	}
	if !valid {
		result.Response.Data = nil
		return result, errors.New("invalid company source metadata")
	}
	return result, nil
}
func read[T any](ctx context.Context, c *Client, path string) (Result[T], error) {
	var result Result[T]
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.Origin+path, nil)
	if err != nil {
		return result, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return result, unavailableError{err}
	}
	defer resp.Body.Close()
	result.Status = resp.StatusCode
	now := time.Now()
	result.RetryAt = retryAt(resp.Header.Get("Retry-After"), now)
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return result, unavailableError{err}
	}
	if len(body) > MaxBody {
		return result, errors.New("company source response too large")
	}
	if len(body) == 0 && resp.StatusCode != 200 {
		return result, errors.New("company source failed without envelope")
	}
	if err := json.Unmarshal(body, &result.Response); err != nil {
		return result, errors.New("invalid company source response")
	}
	s := result.Response.Source
	if s.RetryAfter > 0 {
		at := now.Add(time.Duration(min(s.RetryAfter, 86400)) * time.Second)
		if result.RetryAt == nil || at.After(*result.RetryAt) {
			result.RetryAt = &at
		}
	}
	if s.AccountID != c.config.AccountID || s.BindingRevision != c.config.Revision {
		result.Response.Data = nil
		return result, errors.New("company source scope mismatch")
	}
	if s.State != "fresh" && s.State != "stale" && s.State != "error" && s.State != "disabled" || s.Complete && (result.Response.Data == nil || s.ObservedAt == nil || s.FreshUntil == nil) || s.ObservedAt != nil && s.ObservedAt.After(now.Add(time.Minute)) {
		result.Response.Data = nil
		return result, errors.New("invalid company source state")
	}
	return result, nil
}
func retryAt(raw string, now time.Time) *time.Time {
	if n, e := strconv.ParseInt(raw, 10, 64); e == nil && n > 0 {
		t := now.Add(time.Duration(min(n, 86400)) * time.Second)
		return &t
	}
	if t, e := http.ParseTime(raw); e == nil && t.After(now) {
		return &t
	}
	return nil
}
