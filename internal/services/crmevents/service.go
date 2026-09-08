package crmevents

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type Config struct {
	Workers           int
	PollInterval      time.Duration
	Window            time.Duration
	Overlap           time.Duration
	Lease             time.Duration
	CallTimeout       time.Duration
	PersistTimeout    time.Duration
	Logger            *slog.Logger
	MaxAttempts       int
	MaxPasses         int
	MaxPages          int
	RetentionBatch    int
	DisableEnrichment bool
	Now               func() time.Time
}

func DefaultConfig() Config {
	return Config{Workers: 2, PollInterval: 5 * time.Minute, Window: time.Hour, Overlap: time.Minute, Lease: 30 * time.Second, CallTimeout: 10 * time.Second, PersistTimeout: 5 * time.Second, Logger: slog.Default(), MaxAttempts: 5, MaxPasses: 3, MaxPages: 1000, RetentionBatch: 1000, Now: time.Now}
}
func normalizeConfig(c Config) Config {
	d := DefaultConfig()
	if c.Workers > 0 {
		d.Workers = min(c.Workers, 8)
	}
	if c.PollInterval > 0 {
		d.PollInterval = c.PollInterval
	}
	if c.Window >= time.Second {
		d.Window = c.Window
	}
	if c.Overlap > 0 {
		d.Overlap = c.Overlap
	}
	if c.Lease > 0 {
		d.Lease = c.Lease
	}
	if c.CallTimeout > 0 {
		d.CallTimeout = c.CallTimeout
	}
	if c.PersistTimeout > 0 {
		d.PersistTimeout = min(c.PersistTimeout, 5*time.Second)
	}
	if c.Logger != nil {
		d.Logger = c.Logger
	}
	if c.MaxAttempts > 0 {
		d.MaxAttempts = c.MaxAttempts
	}
	if c.MaxPasses >= 2 {
		d.MaxPasses = c.MaxPasses
	}
	if c.MaxPages > 0 {
		d.MaxPages = c.MaxPages
	}
	if c.RetentionBatch > 0 {
		d.RetentionBatch = min(c.RetentionBatch, 10000)
	}
	if c.Now != nil {
		d.Now = c.Now
	}
	d.DisableEnrichment = c.DisableEnrichment
	return d
}

// Repository exposes complete owner operations, not SQL or foreign database handles.
type Repository interface {
	Apply(context.Context, serviceapi.Command, serviceapi.Principal) (serviceapi.Operation, error)
	Query(context.Context, serviceapi.Query, serviceapi.Principal) (serviceapi.QueryResult, error)
	GetEvent(context.Context, serviceapi.Principal, string) (serviceapi.Event, error)
	Status(context.Context, serviceapi.Principal) (serviceapi.SyncStatus, error)
	Operation(context.Context, serviceapi.Principal, uuid.UUID) (serviceapi.Operation, error)
	Schedule(context.Context) error
	Claim(context.Context) (Slice, error)
	SavePage(context.Context, Slice, serviceapi.EventPage) error
	Fail(context.Context, Slice, error) error
	ClaimEnrichment(context.Context) (EnrichmentClaim, error)
	SaveEnrichment(context.Context, EnrichmentClaim, []enrichmentSave) error
	FailEnrichment(context.Context, EnrichmentClaim, error) error
	Retain(context.Context) (int64, error)
	MetricsSnapshot(context.Context) (Snapshot, error)
}
type Service struct {
	repository Repository
	policy     serviceapi.Policy
	gateway    serviceapi.Gateway
	cfg        Config
	logMu      sync.Mutex
	lastLog    map[string]time.Time
}

func NewWithRepository(repository Repository, policy serviceapi.Policy, gateway serviceapi.Gateway, cfg Config) *Service {
	return &Service{repository: repository, policy: policy, gateway: gateway, cfg: normalizeConfig(cfg)}
}

var _ serviceapi.CRMEvents = (*Service)(nil)
var ErrLeaseLost = errors.New("CRM Events lease lost")
var ErrNoWork = errors.New("CRM Events has no claimable work")

func (s *Service) authorize(ctx context.Context, auth serviceapi.Auth, action string) (serviceapi.Principal, error) {
	c, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	defer cancel()
	return s.policy.Validate(c, auth, serviceapi.EventsService, action)
}
func normalizeCommand(c serviceapi.Command, now time.Time) (serviceapi.Command, error) {
	if c.CommandID == "" || len(c.CommandID) > 128 {
		return c, serviceapi.Fail(serviceapi.InvalidArgument, "command id required, maximum 128 bytes")
	}
	if c.InitialDays == 0 {
		c.InitialDays = 2
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 7
	}
	if c.InitialDays < 1 || c.InitialDays > 7 || c.RetentionDays < 2 || c.RetentionDays > 30 || c.RetentionDays < c.InitialDays {
		return c, serviceapi.Fail(serviceapi.InvalidArgument, "initial days 1..7; retention days 2..30 and at least initial days")
	}
	switch c.Kind {
	case "enable", "sync", "disable":
		if c.From != 0 || c.To != 0 {
			return c, serviceapi.Fail(serviceapi.InvalidArgument, "range only permitted for backfill")
		}
	case "backfill":
		if c.From <= 0 || c.To <= c.From || c.To-c.From > 31*86400 {
			return c, serviceapi.Fail(serviceapi.InvalidArgument, "backfill must be past, within retention, and at most 31 days")
		}
	default:
		return c, serviceapi.Fail(serviceapi.InvalidArgument, "unknown CRM Events command")
	}
	return c, nil
}
func commandHash(c serviceapi.Command, p serviceapi.Principal) []byte {
	c.Auth = serviceapi.Auth{}
	b, _ := json.Marshal(struct {
		Command  serviceapi.Command
		Scope    serviceapi.Scope
		Actor    int64
		Consumer string
	}{c, p.Scope, p.ActorID, p.Consumer})
	h := sha256.Sum256(b)
	return h[:]
}

func (s *Service) Apply(ctx context.Context, cmd serviceapi.Command) (serviceapi.Operation, error) {
	p, err := s.authorize(ctx, cmd.Auth, serviceapi.ActionSync)
	if err != nil {
		return serviceapi.Operation{}, err
	}
	cmd, err = normalizeCommand(cmd, s.cfg.Now())
	if err != nil {
		return serviceapi.Operation{}, err
	}
	op, err := s.repository.Apply(ctx, cmd, p)
	if err != nil {
		return serviceapi.Operation{}, err
	}
	return serviceapi.CanonicalOperation(op)
}
func (s *Service) Operation(ctx context.Context, req serviceapi.OperationRequest) (serviceapi.Operation, error) {
	p, err := s.authorize(ctx, req.Auth, serviceapi.ActionOperation)
	if err != nil {
		return serviceapi.Operation{}, err
	}
	id, err := uuid.Parse(req.OperationID)
	if err != nil {
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.InvalidArgument, "invalid operation id")
	}
	op, err := s.repository.Operation(ctx, p, id)
	if err != nil {
		return serviceapi.Operation{}, err
	}
	return serviceapi.CanonicalOperation(op)
}
