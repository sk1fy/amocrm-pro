package amostart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/time/rate"
)

const (
	origin       = "https://www.amocrm.ru"
	cabinet      = "/partners/private/"
	bill         = cabinet + "shop/bill"
	maxPageBytes = 2 << 20
)

var (
	ErrUnavailable   = errors.New("amoSTART is unavailable")
	ErrRateLimited   = errors.New("amoSTART rate limit reached")
	ErrNoLicense     = errors.New("amoSTART response has no confirmed license")
	ErrAuthorization = errors.New("amoSTART authorization failed")
)

type Config struct {
	Username, Password                    string
	Contract, PartnerID, UserID, Discount string
}

type License struct {
	Until  time.Time
	Tariff string
}

type Client struct {
	config       Config
	origin       string
	http         http.Client
	slot         chan struct{}
	limit        *rate.Limiter
	sessionUntil time.Time
	zone         *time.Location
}

func New(cfg Config) (*Client, error) {
	return newClient(cfg, origin)
}

func newClient(cfg Config, base string) (*Client, error) {
	if cfg == (Config{}) {
		return nil, nil
	}
	if strings.TrimSpace(cfg.Username) == "" || cfg.Password == "" {
		return nil, errors.New("amoSTART requires both username and password")
	}
	for _, value := range []string{cfg.Contract, cfg.PartnerID, cfg.UserID} {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("amoSTART requires valid contract, partner and user IDs")
		}
	}
	if cfg.Discount == "" {
		cfg.Discount = "0"
	}
	discount, err := strconv.Atoi(cfg.Discount)
	if err != nil || discount < 0 || discount > 100 {
		return nil, errors.New("amoSTART discount must be between 0 and 100")
	}
	zone, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return nil, err
	}
	return &Client{
		config: cfg, origin: base, zone: zone, slot: make(chan struct{}, 1),
		limit: rate.NewLimiter(rate.Every(30*time.Second), 1),
		http: http.Client{
			Timeout: 7 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host || len(via) >= 5 {
					return http.ErrUseLastResponse
				}
				return nil
			},
		},
	}, nil
}

func (c *Client) Lookup(ctx context.Context, accountID int64) (License, error) {
	select {
	case c.slot <- struct{}{}:
		defer func() { <-c.slot }()
	default:
		return License{}, ErrRateLimited
	}
	if !c.limit.Allow() {
		return License{}, ErrRateLimited
	}
	if time.Now().After(c.sessionUntil) {
		if err := c.login(ctx); err != nil {
			return License{}, err
		}
	}

	form := url.Values{
		"account_id": {strconv.FormatInt(accountID, 10)}, "bonus_user": {"on"},
		"USER_CONTROL": {"1"}, "PERIOD_CONTROL": {"0"}, "sum_tarif": {"0"},
		"CONTRACT": {c.config.Contract}, "coupon_code": {""}, "DISCOUNT": {c.config.Discount},
		"PARTNER_ID": {c.config.PartnerID}, "USER_ID": {c.config.UserID},
	}
	page, err := c.request(ctx, http.MethodPost, cabinet+"shop/addbillpartners", "application/x-www-form-urlencoded", form.Encode())
	if err == nil {
		license, parseErr := parseLicense(page, c.zone)
		if parseErr == nil {
			return license, nil
		}
		err = parseErr
	}
	if errors.Is(err, ErrAuthorization) {
		c.sessionUntil = time.Time{}
	}
	return License{}, err
}

func (c *Client) login(ctx context.Context) error {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return ErrUnavailable
	}
	c.http.Jar = jar
	page, err := c.request(ctx, http.MethodGet, cabinet, "", "")
	if err != nil {
		return err
	}
	token := inputValue(page, "name", "csrf_token")
	if token == "" {
		return ErrAuthorization
	}
	body, _ := json.Marshal(map[string]string{"csrf_token": token, "username": c.config.Username, "password": c.config.Password})
	if _, err := c.request(ctx, http.MethodPost, "/oauth2/authorize", "application/json", string(body)); err != nil {
		return err
	}
	if _, err := c.request(ctx, http.MethodGet, bill, "", ""); err != nil {
		return err
	}
	c.sessionUntil = time.Now().Add(time.Hour)
	return nil
}

func (c *Client) request(ctx context.Context, method, path, contentType, body string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.origin+path, strings.NewReader(body))
	if err != nil {
		return nil, ErrUnavailable
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, ErrRateLimited
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, ErrAuthorization
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, ErrUnavailable
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	if err != nil || len(page) > maxPageBytes {
		return nil, ErrUnavailable
	}
	return page, nil
}

func inputValue(page []byte, key, value string) string {
	tokens := html.NewTokenizer(bytes.NewReader(page))
	for {
		switch tokens.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokens.Token()
			if token.Data == "input" && attribute(token.Attr, key) == value {
				return strings.TrimSpace(attribute(token.Attr, "value"))
			}
		}
	}
}

func attribute(attrs []html.Attribute, key string) string {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func labeledInput(doc *html.Node, target string) string {
	for node := range doc.Descendants() {
		if node.Data != "label" || attribute(node.Attr, "for") != target || node.Parent == nil || node.Parent.Parent == nil {
			continue
		}
		for field := range node.Parent.Parent.ChildNodes() {
			if field.Data == "div" && hasClass(field, "row_field") {
				return fieldInput(field)
			}
		}
	}
	return ""
}

func hasClass(node *html.Node, want string) bool {
	for class := range strings.FieldsSeq(attribute(node.Attr, "class")) {
		if class == want {
			return true
		}
	}
	return false
}

func fieldInput(field *html.Node) string {
	for node := range field.ChildNodes() {
		if node.Data == "input" {
			return strings.TrimSpace(attribute(node.Attr, "value"))
		}
	}
	return ""
}

func parseLicense(page []byte, zone *time.Location) (License, error) {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return License{}, ErrNoLicense
	}
	period := strings.Join(strings.Fields(labeledInput(doc, "payment_period_end")), " ")
	_, end, found := strings.Cut(" "+period, " до ")
	if !found {
		if period == "" && inputValue(page, "name", "csrf_token") != "" {
			return License{}, ErrAuthorization
		}
		return License{}, ErrNoLicense
	}
	var tariff string
	for field := range doc.Descendants() {
		if field.Data == "div" && hasClass(field, "tariff_value") {
			tariff = fieldInput(field)
			break
		}
	}
	if tariff == "" {
		return License{}, ErrNoLicense
	}
	for _, layout := range []string{"02.01.2006 15:04:05", "02.01.2006 15:04", "02.01.2006"} {
		until, err := time.ParseInLocation(layout, end, zone)
		if err == nil && until.Year() >= 2000 {
			return License{Until: until, Tariff: tariff}, nil
		}
	}
	return License{}, ErrNoLicense
}
