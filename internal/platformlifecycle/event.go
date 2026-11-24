// Package platformlifecycle defines the Core-to-Platform V1 wire values.
package platformlifecycle

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"
)

const (
	EventsPath       = "/internal/platform/v1/core-events"
	CoreIdentity     = "spiffe://amocrm-pro/core"
	PlatformIdentity = "spiffe://amocrm-pro/platform"
	MaxBody          = 8192
	MaxPosition      = 9007199254740991
)

type Kind string

const (
	Installed            Kind = "installed"
	Reauthorized         Kind = "reauthorized"
	Uninstalled          Kind = "uninstalled"
	Reinstalled          Kind = "reinstalled"
	AuthorizationChanged Kind = "authorization_changed"
)

type Authorization string

const (
	Authorized     Authorization = "authorized"
	ReauthRequired Authorization = "reauth_required"
	Disabled       Authorization = "disabled"
	Unknown        Authorization = "unknown"
)

type Event struct {
	InstalledBy   string        `json:"installed_by,omitempty"`
	Version       int           `json:"schema_version"`
	Account       string        `json:"amo_account_id"`
	Integration   uuid.UUID     `json:"integration_id"`
	Installation  uuid.UUID     `json:"core_installation_id"`
	ID            uuid.UUID     `json:"event_id"`
	Kind          Kind          `json:"kind"`
	OccurredAt    time.Time     `json:"occurred_at"`
	Generation    int64         `json:"generation"`
	Sequence      int64         `json:"sequence"`
	Authorization Authorization `json:"authorization,omitempty"`
	Evidence      Evidence      `json:"evidence"`
}

type Evidence struct {
	Source string    `json:"source"`
	ID     uuid.UUID `json:"ref"`
}

type Receipt struct {
	EventID uuid.UUID `json:"event_id"`
	State   string    `json:"state"`
}

func AccountID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 || strconv.FormatInt(id, 10) != raw {
		return 0, errors.New("invalid amoCRM account ID")
	}
	return id, nil
}

func DecodeEvent(raw []byte) (Event, error) {
	var event Event
	if err := Decode(raw, &event); err != nil {
		return event, err
	}
	if _, err := AccountID(event.Account); err != nil {
		return event, err
	}
	if event.Version != 1 || event.Integration == uuid.Nil || event.Installation == uuid.Nil || event.ID == uuid.Nil ||
		event.OccurredAt.IsZero() || event.Generation < 1 || event.Generation > event.Sequence ||
		event.Sequence > MaxPosition || event.Evidence.ID == uuid.Nil {
		return event, errors.New("invalid lifecycle event")
	}
	if event.InstalledBy != "" {
		if _, err := AccountID(event.InstalledBy); err != nil || (event.Kind != Installed && event.Kind != Reinstalled) {
			return event, errors.New("invalid installer identity")
		}
	}
	var source string
	switch event.Kind {
	case Installed, Reauthorized, Reinstalled:
		source = "core_oauth"
	case Uninstalled:
		source = "core_uninstall"
	case AuthorizationChanged:
		source = "core_policy"
		switch event.Authorization {
		case Authorized, ReauthRequired, Disabled, Unknown:
		default:
			return event, errors.New("invalid authorization")
		}
	default:
		return event, errors.New("unknown lifecycle event kind")
	}
	if event.Evidence.Source != source || (event.Kind != AuthorizationChanged && event.Authorization != "") {
		return event, errors.New("invalid lifecycle evidence or authorization")
	}
	return event, nil
}

func Decode(raw []byte, target any) error {
	if len(raw) > MaxBody {
		return errors.New("payload size exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
