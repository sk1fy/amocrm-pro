package amocrm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxOAuthResponseBody = 1 << 20

type OAuthCredentials struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

type OAuthToken struct {
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	ServerTime   int64  `json:"server_time"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type Account struct {
	ID        int64  `json:"id"`
	Subdomain string `json:"subdomain"`
}

type OAuthClient struct {
	httpClient     *http.Client
	resolveAccount func(string) (*url.URL, error)
}

func NewOAuthClient(httpClient *http.Client) *OAuthClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &OAuthClient{httpClient: httpClient, resolveAccount: AccountBaseURL}
}

func (c *OAuthClient) ExchangeCode(
	ctx context.Context,
	accountDomain string,
	credentials OAuthCredentials,
	code string,
) (OAuthToken, error) {
	return c.requestToken(ctx, accountDomain, map[string]string{
		"client_id":     credentials.ClientID,
		"client_secret": credentials.ClientSecret,
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  credentials.RedirectURI,
	})
}

func (c *OAuthClient) Refresh(
	ctx context.Context,
	accountDomain string,
	credentials OAuthCredentials,
	refreshToken string,
) (OAuthToken, error) {
	return c.requestToken(ctx, accountDomain, map[string]string{
		"client_id":     credentials.ClientID,
		"client_secret": credentials.ClientSecret,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"redirect_uri":  credentials.RedirectURI,
	})
}

func (c *OAuthClient) GetAccount(ctx context.Context, accountDomain, accessToken string) (Account, error) {
	baseURL, err := c.resolveAccount(accountDomain)
	if err != nil {
		return Account{}, err
	}
	endpoint := baseURL.ResolveReference(&url.URL{Path: "/api/v4/account"})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Account{}, fmt.Errorf("create account request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return Account{}, fmt.Errorf("get amoCRM account: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxOAuthResponseBody))
		return Account{}, classifyResponse(response.StatusCode, response.Header, time.Now())
	}

	var account Account
	if err := decodeLimitedJSON(response.Body, maxOAuthResponseBody, &account); err != nil {
		return Account{}, fmt.Errorf("decode amoCRM account: %w", err)
	}
	if account.ID <= 0 || strings.TrimSpace(account.Subdomain) == "" {
		return Account{}, errors.New("amoCRM account response is incomplete")
	}
	return account, nil
}

func (c *OAuthClient) requestToken(ctx context.Context, accountDomain string, payload any) (OAuthToken, error) {
	baseURL, err := c.resolveAccount(accountDomain)
	if err != nil {
		return OAuthToken{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return OAuthToken{}, fmt.Errorf("encode OAuth request: %w", err)
	}
	endpoint := baseURL.ResolveReference(&url.URL{Path: "/oauth2/access_token"})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return OAuthToken{}, fmt.Errorf("create OAuth request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return OAuthToken{}, fmt.Errorf("request amoCRM OAuth token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxOAuthResponseBody))
		return OAuthToken{}, classifyResponse(response.StatusCode, response.Header, time.Now())
	}

	var token OAuthToken
	if err := decodeLimitedJSON(response.Body, maxOAuthResponseBody, &token); err != nil {
		return OAuthToken{}, fmt.Errorf("decode amoCRM OAuth token: %w", err)
	}
	if token.AccessToken == "" || token.RefreshToken == "" || token.ExpiresIn <= 0 {
		return OAuthToken{}, errors.New("amoCRM OAuth token response is incomplete")
	}
	return token, nil
}

func decodeLimitedJSON(body io.Reader, limit int64, target any) error {
	limited := io.LimitReader(body, limit+1)
	contents, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if int64(len(contents)) > limit {
		return errors.New("response body exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("response contains multiple JSON values")
	}
	return nil
}
