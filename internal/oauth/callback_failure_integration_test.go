package oauth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCallbackConsumedFailuresRequireRestart(t *testing.T) {
	for _, widget := range []bool{false, true} {
		for _, stage := range []string{"exchange", "account", "save", "deadline", "canceled"} {
			t.Run(stage+map[bool]string{false: "/state", true: "/widget"}[widget], func(t *testing.T) {
				pool := testkitPostgresForServiceCallback(t)
				keys := oauthTestKeyRing(t)
				store := NewStore(pool, keys)
				integration := oauthTestIntegration(t, store)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				cause := errors.New("synthetic dependency failure")
				var exchanges atomic.Int32
				gateway := &serviceCallbackGateway{
					exchangeCode: func(requestCtx context.Context, _, _, _, _, _ string) (Token, error) {
						exchanges.Add(1)
						switch stage {
						case "exchange":
							return Token{}, cause
						case "deadline":
							<-requestCtx.Done()
							return Token{}, requestCtx.Err()
						case "canceled":
							cancel()
							return Token{}, requestCtx.Err()
						}
						return Token{AccessToken: "synthetic", RefreshToken: "synthetic", ExpiresIn: 3600}, nil
					},
					getAccount: func(context.Context, string, string) (Account, error) {
						if stage == "account" {
							return Account{}, cause
						}
						if stage == "save" {
							cancel()
						}
						return Account{ID: 501, Subdomain: "fixture"}, nil
					},
				}
				service, err := NewService(store, keys, gateway, time.Minute, 20*time.Millisecond, InstallLimits{Rate: 2, Burst: 8, InactiveTTL: time.Minute, MaxEntries: 10})
				if err != nil {
					t.Fatal(err)
				}
				state, _, err := store.CreateState(context.Background(), integration.ID, "", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				var observed error
				call := func(callCtx context.Context, _, _, _ string) (InstallationResult, error) {
					var result InstallationResult
					if widget {
						result, observed = service.InstallCallback(callCtx, integration.ClientID, "one-use", "fixture.amocrm.ru")
					} else {
						result, observed = service.Callback(callCtx, state, "one-use", "fixture.amocrm.ru")
					}
					return result, observed
				}
				h := testHandler(fakeFlow{callback: call, install: call}, nil)
				query := "state=" + url.QueryEscape(state) + "&code=one-use&referer=fixture.amocrm.ru"
				if widget {
					query = "from_widget=1&client_id=" + integration.ClientID + "&code=one-use&referer=fixture.amocrm.ru"
				}
				w := requestIDRecorder()
				h.Callback(w, httptest.NewRequest(http.MethodGet, "/oauth/amocrm/callback?"+query, nil).WithContext(ctx))
				_, message, _, retryable := decodeOAuthError(t, w.Body.Bytes())
				if w.Code != http.StatusBadGateway || retryable || message != "authorization failed; restart authorization" || callbackOutcomeOf(observed) != callbackConsumed {
					t.Fatalf("consumed failure status=%d body=%s err=%v", w.Code, w.Body, observed)
				}
				if stage == "deadline" {
					cause = context.DeadlineExceeded
				} else if stage == "canceled" || stage == "save" {
					cause = context.Canceled
				}
				if !errors.Is(observed, cause) {
					t.Fatalf("lost cause %v: %v", cause, observed)
				}
				var replay error
				if widget {
					_, replay = service.InstallCallback(context.Background(), integration.ClientID, "one-use", "fixture.amocrm.ru")
				} else {
					_, replay = service.Callback(context.Background(), state, "one-use", "fixture.amocrm.ru")
				}
				if !errors.Is(replay, ErrInvalidState) || exchanges.Load() != 1 {
					t.Fatalf("replay=%v exchanges=%d", replay, exchanges.Load())
				}
				var installations int
				if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM installations").Scan(&installations); err != nil || installations != 0 {
					t.Fatalf("installations=%d err=%v", installations, err)
				}
			})
		}
	}
}

type lostCallbackReplyConn struct {
	net.Conn
	drop *atomic.Bool
}

func (c lostCallbackReplyConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && c.drop.CompareAndSwap(true, false) {
		_ = c.Conn.Close()
		return 0, io.ErrUnexpectedEOF
	}
	return n, err
}

func TestCallbackLostMutationReplyIsTerminal(t *testing.T) {
	for _, widget := range []bool{false, true} {
		t.Run(map[bool]string{false: "state", true: "widget"}[widget], func(t *testing.T) {
			pool := testkitPostgresForServiceCallback(t)
			keys := oauthTestKeyRing(t)
			store := NewStore(pool, keys)
			integration := oauthTestIntegration(t, store)
			state, _, err := store.CreateState(context.Background(), integration.ID, "", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			var drop atomic.Bool
			config := pool.Config()
			config.MaxConns = 1
			config.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return false }
			config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
			config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return lostCallbackReplyConn{Conn: conn, drop: &drop}, nil
			}
			lostPool, err := pgxpool.NewWithConfig(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer lostPool.Close()
			if err := lostPool.Ping(context.Background()); err != nil {
				t.Fatal(err)
			}
			cipher := installCallbackCipher{Cipher: keys, beforeOpen: func() { drop.Store(true) }}
			var exchanges atomic.Int32
			gateway := &serviceCallbackGateway{exchangeCode: func(context.Context, string, string, string, string, string) (Token, error) {
				exchanges.Add(1)
				return Token{}, errors.New("unexpected exchange")
			}}
			service, err := NewService(NewStore(lostPool, keys), cipher, gateway, time.Minute, time.Second, InstallLimits{Rate: 2, Burst: 8, InactiveTTL: time.Minute, MaxEntries: 10})
			if err != nil {
				t.Fatal(err)
			}
			var observed error
			call := func(ctx context.Context, _, _, _ string) (InstallationResult, error) {
				var result InstallationResult
				if widget {
					result, observed = service.InstallCallback(ctx, integration.ClientID, "lost-reply", "fixture.amocrm.ru")
				} else {
					drop.Store(true)
					result, observed = service.Callback(ctx, state, "lost-reply", "fixture.amocrm.ru")
				}
				return result, observed
			}
			h := testHandler(fakeFlow{callback: call, install: call}, nil)
			query := "state=" + url.QueryEscape(state) + "&code=lost-reply&referer=fixture.amocrm.ru"
			if widget {
				query = "from_widget=1&client_id=" + integration.ClientID + "&code=lost-reply&referer=fixture.amocrm.ru"
			}
			w := requestIDRecorder()
			h.Callback(w, httptest.NewRequest(http.MethodGet, "/oauth/amocrm/callback?"+query, nil))
			_, message, _, retryable := decodeOAuthError(t, w.Body.Bytes())
			if w.Code != http.StatusBadGateway || retryable || message != "authorization failed; restart authorization" || callbackOutcomeOf(observed) != callbackUncertain || drop.Load() {
				t.Fatalf("uncertain mutation status=%d body=%s err=%v", w.Code, w.Body, observed)
			}
			if widget {
				if claimed, err := store.HasInstallCodeClaim(context.Background(), "lost-reply"); err != nil || !claimed {
					t.Fatalf("lost reply did not commit claim: claimed=%v err=%v", claimed, err)
				}
				if err := store.ClaimInstallCode(context.Background(), integration.ID, "lost-reply", installCodeTTL); !errors.Is(err, ErrInvalidState) {
					t.Fatalf("lost claim replay=%v", err)
				}
			} else if _, err := store.ConsumeState(context.Background(), state); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("lost reply did not consume state: %v", err)
			}
			if exchanges.Load() != 0 {
				t.Fatalf("uncertain claim reached exchange %d times", exchanges.Load())
			}
		})
	}
}

func TestCallbackCanceledBeforeMutationRemainsRepeatable(t *testing.T) {
	for _, widget := range []bool{false, true} {
		t.Run(map[bool]string{false: "state", true: "widget"}[widget], func(t *testing.T) {
			pool := testkitPostgresForServiceCallback(t)
			keys := oauthTestKeyRing(t)
			store := NewStore(pool, keys)
			integration := oauthTestIntegration(t, store)
			state, _, err := store.CreateState(context.Background(), integration.ID, "", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			service, err := NewService(store, keys, &serviceCallbackGateway{}, time.Minute, time.Second, InstallLimits{Rate: 2, Burst: 8, InactiveTTL: time.Minute, MaxEntries: 10})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if widget {
				_, err = service.InstallCallback(ctx, integration.ClientID, "cancel-before-claim", "fixture.amocrm.ru")
			} else {
				_, err = service.Callback(ctx, state, "cancel-before-consume", "fixture.amocrm.ru")
			}
			if !errors.Is(err, context.Canceled) || callbackOutcomeOf(err) != callbackRepeatable {
				t.Fatalf("pre-mutation cancellation=%v outcome=%d", err, callbackOutcomeOf(err))
			}
			if widget {
				if err := store.ClaimInstallCode(context.Background(), integration.ID, "cancel-before-claim", installCodeTTL); err != nil {
					t.Fatalf("cancellation consumed code: %v", err)
				}
			} else if _, err := store.ConsumeState(context.Background(), state); err != nil {
				t.Fatalf("cancellation consumed state: %v", err)
			}
		})
	}
}

func TestCallbackStateDecodeFailureIsConsumed(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	state, _, err := store.CreateState(context.Background(), integration.ID, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE integrations SET webhook_events='[1]' WHERE id=$1`, integration.ID); err != nil {
		t.Fatal(err)
	}
	_, err = store.ConsumeState(context.Background(), state)
	if err == nil || callbackOutcomeOf(err) != callbackConsumed {
		t.Fatalf("post-consumption metadata decode failure=%v outcome=%d", err, callbackOutcomeOf(err))
	}
	if _, err := store.ConsumeState(context.Background(), state); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("decode failure reopened state: %v", err)
	}
}
