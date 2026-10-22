package colorleads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ImportGuard is a privileged command boundary, never exposed as a widget
// endpoint. It locks the authenticated operator's scope, frozen transfer,
// installation generation and routing epoch through commit. It MUST reject
// active routes and incomplete old-owner/browser drain evidence.
type ImportGuard interface {
	AdmitImport(context.Context, pgx.Tx, ImportCommand) error
}
type Snapshot struct {
	Settings json.RawMessage `json:"settings"`
	Colors   []Color         `json:"colors"`
}
type ImportCommand struct {
	InstallationID uuid.UUID
	Generation     int64
	Epoch          int64
	TransferID     string
	Actor          string
	SourceDigest   string
	Snapshot       Snapshot
}
type ImportResult struct {
	Digest   string `json:"digest"`
	Replayed bool   `json:"replayed"`
}

// ImportSnapshot is replace-once-per-transfer. It never activates Core or
// manufactures drain receipts. A retry returns the original durable digest;
// changed content for the same transfer is rejected. No widget JWT is used.
func (s *Store) ImportSnapshot(ctx context.Context, guard ImportGuard, c ImportCommand) (ImportResult, error) {
	out := ImportResult{}
	if s == nil || s.pool == nil || guard == nil {
		return out, ErrNotConfigured
	}
	if c.InstallationID == uuid.Nil || c.Generation <= 0 || c.Epoch <= 0 || c.TransferID == "" || len(c.TransferID) > 128 || c.Actor == "" || ValidateSettings(c.Snapshot.Settings) != nil || ValidateColors(c.Snapshot.Colors) != nil {
		return out, ErrInvalid
	}
	sourceHash, hashErr := hex.DecodeString(c.SourceDigest)
	if hashErr != nil || len(sourceHash) != sha256.Size {
		return out, ErrInvalid
	}
	raw, err := json.Marshal(c.Snapshot)
	if err != nil {
		return out, ErrInvalid
	}
	digest := sha256.Sum256(raw)
	out.Digest = hex.EncodeToString(digest[:])
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	if err = guard.AdmitImport(ctx, tx, c); err != nil {
		return out, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO color_leads_imports(installation_id,installation_generation,transfer_id,digest,routing_epoch,actor,source_digest) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, c.InstallationID, c.Generation, c.TransferID, out.Digest, c.Epoch, c.Actor, c.SourceDigest)
	if err != nil {
		return out, err
	}
	if tag.RowsAffected() == 0 {
		var previous, sourceDigest string
		var epoch int64
		err = tx.QueryRow(ctx, `SELECT digest,routing_epoch,source_digest FROM color_leads_imports WHERE installation_id=$1 AND installation_generation=$2 AND transfer_id=$3`, c.InstallationID, c.Generation, c.TransferID).Scan(&previous, &epoch, &sourceDigest)
		if err != nil {
			return out, err
		}
		if previous != out.Digest || epoch != c.Epoch || sourceDigest != c.SourceDigest {
			return out, ErrRevisionConflict
		}
		out.Replayed = true
		return out, tx.Commit(ctx)
	}
	colors, _ := json.Marshal(c.Snapshot.Colors)
	if c.Snapshot.Colors == nil {
		colors = []byte(`[]`)
	}
	// Refuse to overwrite product activity or an earlier import. A subsequent
	// transfer needs an explicit reconciled import policy, not silent replacement.
	tag, err = tx.Exec(ctx, `INSERT INTO color_leads_documents(installation_id,installation_generation,settings,settings_revision,colors,colors_revision) VALUES($1,$2,$3,1,$4,1) ON CONFLICT DO NOTHING`, c.InstallationID, c.Generation, c.Snapshot.Settings, colors)
	if err != nil {
		return out, err
	}
	if tag.RowsAffected() != 1 {
		return out, ErrRevisionConflict
	}
	return out, tx.Commit(ctx)
}
