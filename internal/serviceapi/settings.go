package serviceapi

func DefaultSettings() Settings { return Settings{InitialDays: 2, RetentionDays: 7} }

func ValidateSettings(s Settings) error {
	if s.InitialDays < 1 || s.InitialDays > 7 || s.RetentionDays < 2 || s.RetentionDays > 30 || s.InitialDays > s.RetentionDays {
		return Fail(InvalidArgument, "initial_days must be 1..7, retention_days 2..30 and at least initial_days")
	}
	return nil
}
