package productplatform

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type portraitNiche struct {
	ID      uuid.UUID `json:"id"`
	Name    string    `json:"name"`
	Version int64     `json:"version"`
}

type portraitNichePage struct {
	Items      []portraitNiche `json:"items"`
	Next       *string         `json:"next_cursor"`
	SnapshotAt time.Time       `json:"snapshot_at"`
	QueryHash  string          `json:"query_hash"`
}

type portraitNicheFilter struct {
	Q     string
	Limit int
}

type portraitNicheSeek struct {
	Version int64
	At      time.Time
	Offset  int
}

func (m managerHTTP) listPortraitNiches(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	filter, cursor, err := parsePortraitNicheFilter(r.URL.RawQuery)
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	raw, _ := json.Marshal(filter)
	hash := sha256.Sum256(raw)
	var seek portraitNicheSeek
	if cursor != "" {
		seek, err = m.options.parsePortraitNicheCursor(cursor, env.Actor, hash)
		if err != nil {
			writeManagerStoreError(w, env.RequestID, err)
			return
		}
	}
	page, next, err := m.store.readPortraitNiches(r.Context(), filter, seek)
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	page.QueryHash = hex.EncodeToString(hash[:])
	if next.Offset != 0 {
		cursor = m.options.portraitNicheCursor(next, env.Actor, hash)
		page.Next = &cursor
	}
	writeLifecycleJSON(w, http.StatusOK, page)
}

func parsePortraitNicheFilter(raw string) (portraitNicheFilter, string, error) {
	f := portraitNicheFilter{Limit: 25}
	values, err := boundedAccountQuery(raw)
	if err != nil {
		return f, "", err
	}
	var cursor string
	for key, entries := range values {
		if len(entries) != 1 || !managerText(entries[0], 0, 200) {
			return f, "", errManagerInput
		}
		switch key {
		case "q":
			f.Q = strings.TrimSpace(entries[0])
		case "limit":
			f.Limit, err = accountInteger(entries[0], 1, 100)
			if err != nil {
				return f, "", err
			}
		case "cursor":
			cursor = entries[0]
		default:
			return f, "", errManagerInput
		}
	}
	return f, cursor, nil
}

func (o ManagerOptions) portraitNicheCursor(seek portraitNicheSeek, actor managerActor, hash [32]byte) string {
	var body [24]byte
	binary.BigEndian.PutUint64(body[:8], uint64(seek.Version))
	binary.BigEndian.PutUint64(body[8:16], uint64(seek.At.UnixMicro()))
	binary.BigEndian.PutUint64(body[16:], uint64(seek.Offset))
	return base64.RawURLEncoding.EncodeToString(append(body[:], o.routeCursorMAC("platform-portrait-niches-v1", body[:], actor, hash)...))
}

func (o ManagerOptions) parsePortraitNicheCursor(raw string, actor managerActor, hash [32]byte) (portraitNicheSeek, error) {
	var seek portraitNicheSeek
	body, err := managerBase64(raw)
	if err != nil || len(body) != 24+sha256.Size ||
		!hmac.Equal(body[24:], o.routeCursorMAC("platform-portrait-niches-v1", body[:24], actor, hash)) {
		return seek, errManagerCursor
	}
	seek.Version = int64(binary.BigEndian.Uint64(body[:8]))
	seek.At = time.UnixMicro(int64(binary.BigEndian.Uint64(body[8:16]))).UTC()
	seek.Offset = int(binary.BigEndian.Uint64(body[16:24]))
	if seek.Version <= 0 || seek.Offset <= 0 {
		return seek, errManagerCursor
	}
	return seek, nil
}

func (s managerStore) readPortraitNiches(ctx context.Context, f portraitNicheFilter, seek portraitNicheSeek) (portraitNichePage, portraitNicheSeek, error) {
	var page portraitNichePage
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return page, portraitNicheSeek{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var version int64
	if err := tx.QueryRow(ctx, `SELECT version,statement_timestamp() FROM platform_manager.portrait_niche_revision`).
		Scan(&version, &page.SnapshotAt); err != nil {
		return page, portraitNicheSeek{}, err
	}
	page.SnapshotAt = page.SnapshotAt.UTC()
	if seek.Offset > 0 {
		if seek.Version != version || !page.SnapshotAt.Before(seek.At.Add(managerSnapshotTTL)) {
			return page, portraitNicheSeek{}, errManagerCursor
		}
		page.SnapshotAt = seek.At
	}
	rows, err := tx.Query(ctx, `SELECT id,name,version FROM platform_manager.portrait_niches
		WHERE removed_at IS NULL AND ($1='' OR strpos(lower(name),lower($1))>0)
		ORDER BY lower(name),id OFFSET $2 LIMIT $3`, f.Q, seek.Offset, f.Limit+1)
	if err != nil {
		return page, portraitNicheSeek{}, err
	}
	page.Items, err = pgx.CollectRows(rows, pgx.RowToStructByPos[portraitNiche])
	if err != nil {
		return page, portraitNicheSeek{}, err
	}
	var next portraitNicheSeek
	if len(page.Items) > f.Limit {
		page.Items = page.Items[:f.Limit]
		next = portraitNicheSeek{Version: version, At: page.SnapshotAt, Offset: seek.Offset + f.Limit}
	}
	return page, next, tx.Commit(ctx)
}

type portraitNicheInput struct {
	Name string
}

type portraitNicheMergeInput struct {
	Target  uuid.UUID
	Version int64
}

func (m managerHTTP) createPortraitNiche(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	cmd, err := portraitCommand(r, env, portraitNicheCreateCommandKind, false)
	m.savePortraitNiche(w, r, env, cmd, err)
}

func (m managerHTTP) patchPortraitNiche(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	cmd, err := portraitNicheCommand(r, env, portraitNicheUpdateCommandKind)
	m.savePortraitNiche(w, r, env, cmd, err)
}

func (m managerHTTP) savePortraitNiche(w http.ResponseWriter, r *http.Request, env managerEnvelope, cmd managerCommand, commandErr error) {
	raw, err := readPortraitBody(w, r)
	input, parseErr := parsePortraitNicheInput(raw)
	if commandErr != nil || err != nil || parseErr != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	out, err := m.store.mutate(r.Context(), cmd, input, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		return savePortraitNiche(ctx, tx, cmd, input)
	})
	writePortraitOutcome(w, env.RequestID, out, err)
}

func savePortraitNiche(ctx context.Context, tx pgx.Tx, cmd managerCommand, input portraitNicheInput) (managerOutcome, error) {
	if err := lockNicheDictionary(ctx, tx); err != nil {
		return managerOutcome{}, err
	}
	creating := cmd.Kind == portraitNicheCreateCommandKind
	current := portraitNiche{ID: cmd.ID, Name: input.Name, Version: 1}
	if !creating {
		var err error
		current, err = lockPortraitNiche(ctx, tx, cmd.Resource)
		if err != nil {
			return managerOutcome{}, err
		}
		if cmd.Version != current.Version {
			return managerVersionConflict(cmd, current.Version)
		}
		if current.Name == input.Name {
			return managerMutationResponse(http.StatusOK, current.Version, current)
		}
	}
	var duplicate bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM platform_manager.portrait_niches
		WHERE removed_at IS NULL AND lower(name)=lower($1) AND id<>$2)`, input.Name, current.ID).Scan(&duplicate); err != nil {
		return managerOutcome{}, err
	}
	if duplicate {
		return portraitNicheConflict(cmd)
	}
	status := http.StatusCreated
	if creating {
		_, err := tx.Exec(ctx, `INSERT INTO platform_manager.portrait_niches(id,name) VALUES($1,$2)`, current.ID, input.Name)
		if err != nil {
			return managerOutcome{}, err
		}
	} else {
		status = http.StatusOK
		current.Name = input.Name
		err := tx.QueryRow(ctx, `UPDATE platform_manager.portrait_niches SET name=$2,version=version+1
			WHERE id=$1 RETURNING version`, current.ID, current.Name).Scan(&current.Version)
		if err != nil {
			return managerOutcome{}, err
		}
	}
	if err := advanceNicheDictionary(ctx, tx); err != nil {
		return managerOutcome{}, err
	}
	return managerMutationResponse(status, current.Version, current)
}

func (m managerHTTP) deletePortraitNiche(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	cmd, err := portraitNicheCommand(r, env, portraitNicheDeleteCommandKind)
	body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
	if err != nil || readErr != nil || len(body) != 0 {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	out, err := m.store.mutate(r.Context(), cmd, nil, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		if err := lockNicheDictionary(ctx, tx); err != nil {
			return managerOutcome{}, err
		}
		current, err := lockPortraitNiche(ctx, tx, cmd.Resource)
		if err != nil {
			return managerOutcome{}, err
		}
		if cmd.Version != current.Version {
			return managerVersionConflict(cmd, current.Version)
		}
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM platform_manager.account_portraits WHERE niche_id=$1)`,
			current.ID).Scan(&used); err != nil {
			return managerOutcome{}, err
		}
		if used {
			return portraitNicheConflict(cmd)
		}
		if err := removePortraitNiche(ctx, tx, current.ID, nil); err != nil {
			return managerOutcome{}, err
		}
		if err := advanceNicheDictionary(ctx, tx); err != nil {
			return managerOutcome{}, err
		}
		return managerMutationResponse(http.StatusNoContent, 0, nil)
	})
	writePortraitOutcome(w, env.RequestID, out, err)
}

func (m managerHTTP) mergePortraitNiche(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	cmd, err := portraitNicheCommand(r, env, portraitNicheMergeCommandKind)
	raw, readErr := readPortraitBody(w, r)
	input, parseErr := parsePortraitNicheMerge(raw)
	if err != nil || readErr != nil || parseErr != nil || input.Target == cmd.Resource {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	out, err := m.store.mutate(r.Context(), cmd, input, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		return mergePortraitNiche(ctx, tx, cmd, input)
	})
	writePortraitOutcome(w, env.RequestID, out, err)
}

func mergePortraitNiche(ctx context.Context, tx pgx.Tx, cmd managerCommand, input portraitNicheMergeInput) (managerOutcome, error) {
	if err := lockNicheDictionary(ctx, tx); err != nil {
		return managerOutcome{}, err
	}
	first, second := cmd.Resource, input.Target
	if first.String() > second.String() {
		first, second = second, first
	}
	left, err := lockPortraitNiche(ctx, tx, first)
	if err != nil {
		return managerOutcome{}, err
	}
	right, err := lockPortraitNiche(ctx, tx, second)
	if err != nil {
		return managerOutcome{}, err
	}
	source, target := left, right
	if source.ID != cmd.Resource {
		source, target = target, source
	}
	if cmd.Version != source.Version {
		return managerVersionConflict(cmd, source.Version)
	}
	if input.Version != target.Version {
		return managerVersionConflict(cmd, target.Version)
	}
	rows, err := tx.Query(ctx, `UPDATE platform_manager.account_portraits SET niche_id=$2,version=version+1
		WHERE niche_id=$1 RETURNING account_id,version`, source.ID, target.ID)
	if err != nil {
		return managerOutcome{}, err
	}
	type changedPortrait struct {
		Account uuid.UUID
		Version int64
	}
	changed, err := pgx.CollectRows(rows, pgx.RowToStructByPos[changedPortrait])
	if err != nil {
		return managerOutcome{}, err
	}
	for _, item := range changed {
		if _, err := recordAccountChange(ctx, tx, cmd, accountChange{
			Account: item.Account, Kind: accountPortraitUpdated, Version: item.Version,
			Details: map[string]any{"old_niche_id": source.ID, "new_niche_id": target.ID},
		}); err != nil {
			return managerOutcome{}, err
		}
	}
	if err := removePortraitNiche(ctx, tx, source.ID, &target.ID); err != nil {
		return managerOutcome{}, err
	}
	err = tx.QueryRow(ctx, `UPDATE platform_manager.portrait_niches SET version=version+1
		WHERE id=$1 RETURNING version`, target.ID).Scan(&target.Version)
	if err != nil {
		return managerOutcome{}, err
	}
	if err := advanceNicheDictionary(ctx, tx); err != nil {
		return managerOutcome{}, err
	}
	return managerMutationResponse(http.StatusOK, target.Version, target)
}

func lockPortraitNiche(ctx context.Context, tx pgx.Tx, id uuid.UUID) (portraitNiche, error) {
	var out portraitNiche
	err := tx.QueryRow(ctx, `SELECT id,name,version FROM platform_manager.portrait_niches
		WHERE id=$1 AND removed_at IS NULL FOR UPDATE`, id).Scan(&out.ID, &out.Name, &out.Version)
	return out, accountReadError(err)
}

func lockNicheDictionary(ctx context.Context, tx pgx.Tx) error {
	var version int64
	return tx.QueryRow(ctx, `SELECT version FROM platform_manager.portrait_niche_revision
		WHERE singleton FOR UPDATE`).Scan(&version)
}

func advanceNicheDictionary(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `UPDATE platform_manager.portrait_niche_revision SET version=version+1 WHERE singleton`)
	return err
}

func removePortraitNiche(ctx context.Context, tx pgx.Tx, id uuid.UUID, target *uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE platform_manager.portrait_niches SET version=version+1,
		removed_at=statement_timestamp(),merged_into=$2 WHERE id=$1`, id, target)
	return err
}

func portraitNicheConflict(cmd managerCommand) (managerOutcome, error) {
	return managerMutationResponse(http.StatusConflict, 0, managerProblem(
		http.StatusConflict, "state_conflict", cmd.RequestID, 0, 0))
}

func portraitNicheCommand(r *http.Request, env managerEnvelope, kind string) (managerCommand, error) {
	cmd, err := portraitCommand(r, env, kind, true)
	if err != nil {
		return cmd, err
	}
	cmd.Resource, err = managerUUID(r.PathValue("id"))
	return cmd, err
}

func parsePortraitNicheInput(raw []byte) (portraitNicheInput, error) {
	var input portraitNicheInput
	fields, err := portraitObject(raw, "name")
	if err != nil {
		return input, err
	}
	ok, err := portraitMember(fields, "name", &input.Name, false)
	if err != nil || !ok || !managerText(input.Name, 1, 1000) {
		return input, errManagerInput
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return input, errManagerInput
	}
	return input, nil
}

func parsePortraitNicheMerge(raw []byte) (portraitNicheMergeInput, error) {
	var input portraitNicheMergeInput
	fields, err := portraitObject(raw, "target_id", "target_version")
	if err != nil {
		return input, err
	}
	input.Target, err = portraitMemberUUID(fields, "target_id")
	if err != nil {
		return input, err
	}
	ok, err := portraitMember(fields, "target_version", &input.Version, false)
	if err != nil || !ok || input.Version < 1 || input.Version > 9007199254740991 {
		return input, errManagerInput
	}
	return input, nil
}
