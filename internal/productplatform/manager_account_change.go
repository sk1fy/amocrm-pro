package productplatform

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	accountProfileUpdated  = "account_profile_updated"
	accountPortraitUpdated = "portrait_updated"
	accountAccessRenewed   = "access_renewed"
)

type accountChange struct {
	Account, Product uuid.UUID
	Kind             string
	Version          int64
	Reason           *string
	Details          any
}

func recordAccountChange(ctx context.Context, tx pgx.Tx, cmd managerCommand, change accountChange) (uuid.UUID, error) {
	id := uuid.New()
	_, err := tx.Exec(ctx, `
		WITH event AS (
			INSERT INTO platform_manager.account_change_events
				(id,command_id,account_id,product_id,kind,actor_id,occurred_at,reason,details,version)
			VALUES($1,$2,$3,$4,$5,$6,statement_timestamp(),$7,COALESCE($8::jsonb,'{}'::jsonb),$9)
			RETURNING id,account_id,product_id,kind,occurred_at,recorded_at
		)
		INSERT INTO platform_manager.account_event_outbox(event_id,payload)
		SELECT id,jsonb_strip_nulls(jsonb_build_object(
			'id',id,'kind',kind,'account_id',account_id,'product_id',product_id,
			'occurred_at',occurred_at,'recorded_at',recorded_at,
			'summary',CASE kind
				WHEN 'account_profile_updated' THEN 'Обновлён профиль клиента'
				WHEN 'portrait_updated' THEN 'Обновлён портрет клиента'
				WHEN 'access_renewed' THEN 'Обновлён срок доступа'
			END,
			'related_refs',jsonb_build_array(jsonb_build_object('kind','account','id',account_id)) ||
				CASE WHEN product_id IS NOT NULL THEN jsonb_build_array(jsonb_build_object('kind','product','id',product_id))
				ELSE '[]'::jsonb END
		)) FROM event`,
		id, cmd.ID, change.Account, commandUUID(change.Product), change.Kind, cmd.Subject, change.Reason, change.Details, change.Version)
	return id, err
}
