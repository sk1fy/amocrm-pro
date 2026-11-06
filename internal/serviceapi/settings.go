package serviceapi

// RetentionDaysMax keeps raw CRM events long enough for a whole previous week
// and the first days of the previous month (period-v1, decision R3). Initial
// history and backfill stay bounded by the 31-day Query span.
const RetentionDaysMax = 33

func DefaultSettings() Settings { return Settings{InitialDays: 31, RetentionDays: RetentionDaysMax} }

func ValidateSettings(s Settings) error {
	if s.InitialDays < 1 || s.InitialDays > 31 || s.RetentionDays < 2 || s.RetentionDays > RetentionDaysMax || s.InitialDays > s.RetentionDays {
		return Fail(InvalidArgument, "initial_days must be 1..31, retention_days 2..33 and at least initial_days")
	}
	return nil
}
