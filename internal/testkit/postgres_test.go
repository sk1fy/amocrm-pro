package testkit

import "testing"

func TestValidateResetTarget(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		allowed string
		wantErr bool
	}{
		{
			name:    "explicit isolated test database",
			url:     "postgres://user:pass@postgres:5432/amocrm_test?sslmode=disable",
			allowed: "true",
		},
		{
			name:    "missing explicit reset permission",
			url:     "postgres://user:pass@postgres:5432/amocrm_test?sslmode=disable",
			wantErr: true,
		},
		{
			name:    "development database rejected",
			url:     "postgres://user:pass@postgres:5432/amocrm?sslmode=disable",
			allowed: "true",
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateResetTarget(test.url, test.allowed)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateResetTarget() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}
