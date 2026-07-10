package webhook

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/installations"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
	"golang.org/x/time/rate"
)

type InstallationFinder interface {
	FindActiveByWebhookKeyHash(context.Context, []byte) (installations.Installation, error)
}

type DeliverySaver interface {
	SaveDeliveryAndEnqueue(context.Context, uuid.UUID, uuid.UUID, string, []byte) (uuid.UUID, error)
	SaveInvalidDelivery(context.Context, uuid.UUID, uuid.UUID, string, []byte, string) (uuid.UUID, error)
}

type Handler struct {
	installations InstallationFinder
	deliveries    DeliverySaver
	logger        *slog.Logger
	maxBody       int64
	databaseTTL   time.Duration
	limiters      sync.Map
	globalLimiter *rate.Limiter
}

func NewHandler(
	installationStore InstallationFinder,
	deliveryStore DeliverySaver,
	logger *slog.Logger,
	maxBody int64,
	databaseTTL time.Duration,
) *Handler {
	return &Handler{
		installations: installationStore,
		deliveries:    deliveryStore,
		logger:        logger,
		maxBody:       maxBody,
		databaseTTL:   databaseTTL,
		globalLimiter: rate.NewLimiter(500, 1_000),
	}
}

func (h *Handler) Receive(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "webhookKey")
	if key == "" {
		http.NotFound(w, r)
		return
	}
	if !h.globalLimiter.Allow() {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}
	keyHash := sha256.Sum256([]byte(key))
	operationContext, cancel := context.WithTimeout(r.Context(), h.databaseTTL)
	defer cancel()
	r = r.WithContext(operationContext)
	if deadline, ok := operationContext.Deadline(); ok {
		_ = http.NewResponseController(w).SetReadDeadline(deadline)
	}

	installation, err := h.installations.FindActiveByWebhookKeyHash(operationContext, keyHash[:])
	if errors.Is(err, installations.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.logger.Error("find webhook installation", "error", err, "request_id", httpmiddleware.RequestIDFromContext(r.Context()))
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if !h.allow(installation.ID) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.maxBody)
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read request body", http.StatusBadRequest)
		return
	}

	accountID, accountOK := AccountID(rawBody)
	if accountOK && accountID != installation.AccountID {
		http.NotFound(w, r)
		return
	}

	requestID := httpmiddleware.RequestIDFromContext(r.Context())
	if !accountOK {
		deliveryID, saveErr := h.deliveries.SaveInvalidDelivery(
			operationContext, installation.ID, requestID, mediaType, rawBody, "missing or invalid account[id]",
		)
		if saveErr != nil {
			h.logger.Error("persist invalid webhook delivery", "error", saveErr, "request_id", requestID, "installation_id", installation.ID)
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		h.logger.Warn("invalid webhook accepted", "request_id", requestID, "delivery_id", deliveryID, "installation_id", installation.ID)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	deliveryID, err := h.deliveries.SaveDeliveryAndEnqueue(
		operationContext,
		installation.ID,
		requestID,
		mediaType,
		rawBody,
	)
	if err != nil {
		h.logger.Error("persist webhook delivery",
			"error", err,
			"request_id", requestID,
			"installation_id", installation.ID,
		)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	h.logger.Info("webhook accepted",
		"request_id", requestID,
		"delivery_id", deliveryID,
		"installation_id", installation.ID,
	)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) allow(installationID uuid.UUID) bool {
	value, _ := h.limiters.LoadOrStore(installationID, rate.NewLimiter(20, 40))
	return value.(*rate.Limiter).Allow()
}
