package productplatform

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

func accountHasTechnicalTariff(ctx context.Context, tx pgx.Tx, account uuid.UUID) (bool, error) {
	var technical bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT FROM platform_manager.account_tariff_fields
		WHERE account_id=$1 AND field='tariff_id' AND numeric_value=$2)`, account, technicalAccountTariffID).Scan(&technical)
	return technical, err
}

func applyTechnicalAccountClassification(ctx context.Context, tx pgx.Tx, account uuid.UUID, tariffID int64, input wire.AccountLicenseRequest) error {
	technical := tariffID == technicalAccountTariffID
	_, err := tx.Exec(ctx, `
		WITH change AS (
			INSERT INTO platform_manager.account_tariff_classification_changes
				(account_id,version,previous,previous_partner_name,current,current_partner_name,
				 reason,tariff_id,installation_id,generation,authorization_sequence,occurred_at)
			SELECT p.account_id,p.version+1,p.classification,p.partner_name,
				CASE WHEN $2 THEN 'Запрет работы' ELSE active.previous END,
				CASE WHEN NOT $2 THEN active.previous_partner_name END,
				CASE WHEN $2 THEN 'technical_tariff_detected' ELSE 'ordinary_tariff_detected' END,
				$3,$4,$5,$6,$7
			FROM platform_manager.account_profiles p
			LEFT JOIN platform_manager.account_tariff_classification_changes active
				ON (active.account_id,active.version)=(p.account_id,p.technical_classification_version)
			WHERE p.account_id=$1 AND (($2 AND p.classification IS DISTINCT FROM 'Запрет работы')
				OR (NOT $2 AND active.account_id IS NOT NULL))
			RETURNING account_id,version,current,current_partner_name
		)
		UPDATE platform_manager.account_profiles p SET
			classification=c.current,partner_name=c.current_partner_name,version=c.version,
			technical_classification_version=CASE WHEN $2 THEN c.version END
		FROM change c WHERE p.account_id=c.account_id`, account, technical, tariffID, input.Scope.Installation,
		input.Scope.Generation, input.Scope.AuthorizationSequence, input.ReceivedAt)
	return err
}
