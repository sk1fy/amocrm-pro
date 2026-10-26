package serviceapi

func DefaultSettings() Settings { return Settings{InitialDays: 31, RetentionDays: 31} }

func ValidateSettings(s Settings) error {
	if s.InitialDays < 1 || s.InitialDays > 31 || s.RetentionDays < 2 || s.RetentionDays > 31 || s.InitialDays > s.RetentionDays {
		return Fail(InvalidArgument, "initial_days must be 1..31, retention_days 2..31 and at least initial_days")
	}
	return nil
}
