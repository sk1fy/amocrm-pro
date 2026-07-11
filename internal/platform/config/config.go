package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const developmentEncryptionKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

type Common struct {
	ServiceName            string
	Environment            string
	LogLevel               string
	DatabaseURL            string
	HTTPAddress            string
	ShutdownTimeout        time.Duration
	DatabaseTimeout        time.Duration
	DBMaxConns             int32
	EncryptionKeys         string
	EncryptionKeyVersion   int
	ExternalRequestTimeout time.Duration
}

type API struct {
	Common
	MaxWebhookBody       int64
	WebhookTimeout       time.Duration
	OAuthStateTTL        time.Duration
	WidgetJWTLeeway      time.Duration
	WidgetJWTMaxLifetime time.Duration
	BootstrapIntegration *BootstrapIntegration
}

type BootstrapIntegration struct {
	Code          string
	ClientID      string
	ClientSecret  string
	RedirectURI   string
	WebhookEvents []string
}

type Worker struct {
	Common
	WorkerID      string
	PollInterval  time.Duration
	LeaseDuration time.Duration
	JobTimeout    time.Duration
	BatchSize     int
	Concurrency   int
	PublicBaseURL string
}

type Migrate struct {
	DatabaseURL   string
	MigrationsDir string
	Timeout       time.Duration
}

func LoadAPI() (API, error) {
	common, err := loadCommon("amocrm-api", ":8080")
	if err != nil {
		return API{}, err
	}

	maxBody, err := int64Value("MAX_WEBHOOK_BODY_BYTES", 2<<20, 1024, 16<<20)
	if err != nil {
		return API{}, err
	}

	webhookTimeout, err := duration("WEBHOOK_TIMEOUT", 1500*time.Millisecond)
	if err != nil {
		return API{}, err
	}
	if webhookTimeout >= 2*time.Second {
		return API{}, errors.New("WEBHOOK_TIMEOUT must be less than amoCRM's 2s delivery deadline")
	}

	oauthStateTTL, err := duration("OAUTH_STATE_TTL", 15*time.Minute)
	if err != nil {
		return API{}, err
	}
	widgetJWTLeeway, err := duration("WIDGET_JWT_LEEWAY", 5*time.Second)
	if err != nil {
		return API{}, err
	}
	widgetJWTMaxLifetime, err := duration("WIDGET_JWT_MAX_LIFETIME", 15*time.Minute)
	if err != nil {
		return API{}, err
	}
	bootstrap, err := loadBootstrapIntegration()
	if err != nil {
		return API{}, err
	}

	return API{
		Common: common, MaxWebhookBody: maxBody, WebhookTimeout: webhookTimeout,
		OAuthStateTTL: oauthStateTTL, WidgetJWTLeeway: widgetJWTLeeway,
		WidgetJWTMaxLifetime: widgetJWTMaxLifetime,
		BootstrapIntegration: bootstrap,
	}, nil
}

func loadBootstrapIntegration() (*BootstrapIntegration, error) {
	code := strings.TrimSpace(os.Getenv("BOOTSTRAP_INTEGRATION_CODE"))
	clientID := strings.TrimSpace(os.Getenv("AMOCRM_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("AMOCRM_CLIENT_SECRET"))
	redirectURI := strings.TrimSpace(os.Getenv("AMOCRM_REDIRECT_URI"))
	if code == "" && clientID == "" && clientSecret == "" && redirectURI == "" {
		return nil, nil
	}
	if code == "" || clientID == "" || clientSecret == "" || redirectURI == "" {
		return nil, errors.New("BOOTSTRAP_INTEGRATION_CODE, AMOCRM_CLIENT_ID, AMOCRM_CLIENT_SECRET, and AMOCRM_REDIRECT_URI must be set together")
	}
	events := make([]string, 0)
	for _, value := range strings.Split(os.Getenv("AMOCRM_WEBHOOK_EVENTS"), ",") {
		if event := strings.TrimSpace(value); event != "" {
			events = append(events, event)
		}
	}
	if len(events) == 0 {
		events = []string{"add_lead", "update_lead", "status_lead", "delete_lead", "add_contact", "update_contact"}
	}
	return &BootstrapIntegration{
		Code: code, ClientID: clientID, ClientSecret: clientSecret,
		RedirectURI: redirectURI, WebhookEvents: events,
	}, nil
}

func LoadWorker() (Worker, error) {
	common, err := loadCommon("amocrm-worker", ":8081")
	if err != nil {
		return Worker{}, err
	}

	pollInterval, err := duration("WORKER_POLL_INTERVAL", time.Second)
	if err != nil {
		return Worker{}, err
	}
	if pollInterval < 100*time.Millisecond {
		return Worker{}, errors.New("WORKER_POLL_INTERVAL must be at least 100ms")
	}
	leaseDuration, err := duration("WORKER_LEASE_DURATION", time.Minute)
	if err != nil {
		return Worker{}, err
	}
	if leaseDuration < 3*time.Second {
		return Worker{}, errors.New("WORKER_LEASE_DURATION must be at least 3s")
	}
	jobTimeout, err := duration("WORKER_JOB_TIMEOUT", 45*time.Second)
	if err != nil {
		return Worker{}, err
	}
	batchSize, err := integer("WORKER_BATCH_SIZE", 10, 1, 100)
	if err != nil {
		return Worker{}, err
	}
	concurrency, err := integer("WORKER_CONCURRENCY", 4, 1, 64)
	if err != nil {
		return Worker{}, err
	}
	publicBaseURL := strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL"))
	if publicBaseURL == "" {
		return Worker{}, errors.New("PUBLIC_BASE_URL is required")
	}

	workerID := strings.TrimSpace(os.Getenv("WORKER_ID"))
	if workerID == "" {
		hostname, hostErr := os.Hostname()
		if hostErr != nil || hostname == "" {
			hostname = "worker"
		}
		workerID = fmt.Sprintf("%s-%d", hostname, os.Getpid())
	}

	return Worker{
		Common:        common,
		WorkerID:      workerID,
		PollInterval:  pollInterval,
		LeaseDuration: leaseDuration,
		JobTimeout:    jobTimeout,
		BatchSize:     batchSize,
		Concurrency:   concurrency,
		PublicBaseURL: publicBaseURL,
	}, nil
}

func LoadMigrate() (Migrate, error) {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return Migrate{}, errors.New("DATABASE_URL is required")
	}

	timeout, err := duration("MIGRATION_TIMEOUT", 2*time.Minute)
	if err != nil {
		return Migrate{}, err
	}

	dir := strings.TrimSpace(os.Getenv("MIGRATIONS_DIR"))
	if dir == "" {
		dir = "/migrations"
	}

	return Migrate{DatabaseURL: databaseURL, MigrationsDir: dir, Timeout: timeout}, nil
}

func loadCommon(serviceName, defaultAddress string) (Common, error) {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return Common{}, errors.New("DATABASE_URL is required")
	}

	shutdownTimeout, err := duration("SHUTDOWN_TIMEOUT", 15*time.Second)
	if err != nil {
		return Common{}, err
	}
	databaseTimeout, err := duration("DATABASE_TIMEOUT", 2*time.Second)
	if err != nil {
		return Common{}, err
	}
	maxConns, err := integer("DB_MAX_CONNS", 10, 1, 100)
	if err != nil {
		return Common{}, err
	}
	encryptionKeys := strings.TrimSpace(os.Getenv("ENCRYPTION_KEYS"))
	if encryptionKeys == "" {
		return Common{}, errors.New("ENCRYPTION_KEYS is required")
	}
	encryptionKeyVersion, err := integer("ACTIVE_ENCRYPTION_KEY_VERSION", 1, 1, 1_000_000)
	if err != nil {
		return Common{}, err
	}
	externalRequestTimeout, err := duration("AMOCRM_REQUEST_TIMEOUT", 10*time.Second)
	if err != nil {
		return Common{}, err
	}

	address := strings.TrimSpace(os.Getenv("HTTP_ADDRESS"))
	if address == "" {
		address = defaultAddress
	}

	environment := strings.TrimSpace(os.Getenv("APP_ENV"))
	if environment == "" {
		environment = "development"
	}
	if environment != "development" && strings.Contains(encryptionKeys, developmentEncryptionKey) {
		return Common{}, errors.New("the public development encryption key is forbidden outside APP_ENV=development")
	}

	logLevel := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL")))
	if logLevel == "" {
		logLevel = "info"
	}
	switch logLevel {
	case "debug", "info", "warn", "error":
	default:
		return Common{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn, or error, got %q", logLevel)
	}

	return Common{
		ServiceName:            serviceName,
		Environment:            environment,
		LogLevel:               logLevel,
		DatabaseURL:            databaseURL,
		HTTPAddress:            address,
		ShutdownTimeout:        shutdownTimeout,
		DatabaseTimeout:        databaseTimeout,
		DBMaxConns:             int32(maxConns),
		EncryptionKeys:         encryptionKeys,
		EncryptionKeyVersion:   encryptionKeyVersion,
		ExternalRequestTimeout: externalRequestTimeout,
	}, nil
}

func duration(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration: %q", name, raw)
	}
	return value, nil
}

func integer(name string, fallback, minValue, maxValue int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue || value > maxValue {
		return 0, fmt.Errorf("%s must be an integer between %d and %d: %q", name, minValue, maxValue, raw)
	}
	return value, nil
}

func int64Value(name string, fallback, minValue, maxValue int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < minValue || value > maxValue {
		return 0, fmt.Errorf("%s must be an integer between %d and %d: %q", name, minValue, maxValue, raw)
	}
	return value, nil
}
