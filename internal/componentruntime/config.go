// Package componentruntime is the composition root for the optional Activity
// service graph. Domain implementations do not import it.
package componentruntime

import (
	"fmt"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Mode                string
	IdentityDir         string
	EmbeddedIdentityDir string
	GatewayAddress      string
	ActivityAddress     string
	EventsAddress       string
	RPCAddress          string
	HealthAddress       string
	ActivityDSN         string
	EventsDSN           string
	ActivityPool        int32
	EventsPool          int32
	EventsWorkers       int
	DisableEnrichment   bool
	PollInterval        time.Duration
	ShareOrigin         string
}

func Load(role string) (Config, error) {
	c := Config{Mode: os.Getenv("ACTIVITY_MODE"), IdentityDir: env("SERVICE_IDENTITY_DIR", "/identity"),
		EmbeddedIdentityDir: env("EMBEDDED_IDENTITY_DIR", "/identities"),
		GatewayAddress:      env("GATEWAY_ADDRESS", "worker:9090"), ActivityAddress: env("ACTIVITY_ADDRESS", "activity:9091"),
		EventsAddress: env("CRM_EVENTS_ADDRESS", "crm-events:9092"), RPCAddress: os.Getenv("SERVICE_RPC_ADDRESS"),
		HealthAddress: os.Getenv("SERVICE_HEALTH_ADDRESS"), ActivityDSN: os.Getenv("ACTIVITY_DATABASE_URL"),
		EventsDSN: os.Getenv("CRM_EVENTS_DATABASE_URL"), ShareOrigin: strings.TrimRight(strings.TrimSpace(os.Getenv("ACTIVITY_APP_PUBLIC_ORIGIN")), "/")}
	if role == "api" || role == "worker" {
		if c.Mode == "" || c.Mode == "off" {
			c.Mode = "off"
			return c, nil
		}
		if c.Mode != "embedded" && c.Mode != "grpc" {
			return c, fmt.Errorf("ACTIVITY_MODE must be off, embedded or grpc")
		}
	} else {
		if c.Mode != "grpc" {
			return c, fmt.Errorf("standalone services require ACTIVITY_MODE=grpc")
		}
		for _, key := range []string{"DATABASE_URL", "ENCRYPTION_KEYS", "AMOCRM_CLIENT_SECRET"} {
			if os.Getenv(key) != "" {
				return c, fmt.Errorf("%s must not be supplied to standalone %s", key, role)
			}
		}
	}
	if c.Mode == "grpc" && (role == "api" || role == "worker") && (c.ActivityDSN != "" || c.EventsDSN != "") {
		return c, fmt.Errorf("Core grpc mode must not receive service DSNs")
	}
	if role == "activity" && c.EventsDSN != "" {
		return c, fmt.Errorf("Activity must not receive CRM Events DSN")
	}
	if role == "crm-events" && c.ActivityDSN != "" {
		return c, fmt.Errorf("CRM Events must not receive Activity DSN")
	}
	var err error
	activityDefaults, _ := services.Describe(services.Activity)
	eventsDefaults, _ := services.Describe(services.CRMEvents)
	if c.ActivityPool, err = poolLimit("ACTIVITY_DB_MAX_CONNS", int32(activityDefaults.MaxConnections)); err != nil {
		return c, err
	}
	if c.EventsPool, err = poolLimit("CRM_EVENTS_DB_MAX_CONNS", int32(eventsDefaults.MaxConnections)); err != nil {
		return c, err
	}
	workers, err := poolLimit("CRM_EVENTS_WORKERS", int32(eventsDefaults.Concurrency))
	if err != nil {
		return c, err
	}
	c.EventsWorkers = int(workers)
	if c.EventsWorkers > 4 {
		return c, fmt.Errorf("CRM_EVENTS_WORKERS exceeds pilot maximum 4")
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CRM_EVENTS_ENRICHMENT"))) {
	case "0", "false", "off":
		c.DisableEnrichment = true
	}
	c.PollInterval = 5 * time.Minute
	if raw := os.Getenv("CRM_EVENTS_POLL_INTERVAL"); raw != "" {
		c.PollInterval, err = time.ParseDuration(raw)
		if err != nil || c.PollInterval < time.Second || c.PollInterval > time.Hour {
			return c, fmt.Errorf("CRM_EVENTS_POLL_INTERVAL must be 1s..1h")
		}
	}
	if (role == "activity" || role == "api" && c.Mode == "embedded") && c.ActivityDSN == "" {
		return c, fmt.Errorf("ACTIVITY_DATABASE_URL required")
	}
	if (role == "crm-events" || role == "api" && c.Mode == "embedded") && c.EventsDSN == "" {
		return c, fmt.Errorf("CRM_EVENTS_DATABASE_URL required")
	}
	if c.RPCAddress == "" {
		c.RPCAddress = map[string]string{"worker": ":9090", "activity": ":9091", "crm-events": ":9092"}[role]
	}
	if c.HealthAddress == "" {
		c.HealthAddress = map[string]string{"activity": ":8091", "crm-events": ":8092"}[role]
	}
	return c, nil
}

func (c Config) Identity(service string) string {
	if c.Mode == "embedded" {
		return filepath.Join(c.EmbeddedIdentityDir, service)
	}
	return c.IdentityDir
}
func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
func poolLimit(key string, fallback int32) (int32, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, e := strconv.Atoi(v)
	if e != nil || n < 1 || n > 16 {
		return 0, fmt.Errorf("%s must be 1..16", key)
	}
	return int32(n), nil
}
