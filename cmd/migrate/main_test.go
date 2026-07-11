package main

import (
	"errors"
	"testing"
)

func TestMigrationCommand(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		confirm   string
		want      string
		wantUsage bool
		wantError bool
	}{
		{name: "default up", want: "up"},
		{name: "explicit up", arguments: []string{"up"}, want: "up"},
		{name: "confirmed down", arguments: []string{"down"}, confirm: downConfirmationValue, want: "down"},
		{name: "missing confirmation", arguments: []string{"down"}, wantError: true},
		{name: "wrong confirmation", arguments: []string{"down"}, confirm: "yes", wantError: true},
		{name: "unknown command", arguments: []string{"status"}, wantUsage: true},
		{name: "too many arguments", arguments: []string{"up", "down"}, wantUsage: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, err := migrationCommand(test.arguments, func(name string) string {
				if name != downConfirmationEnvironment {
					t.Fatalf("unexpected environment lookup %q", name)
				}
				return test.confirm
			})
			if test.wantUsage {
				if !errors.Is(err, errUsage) {
					t.Fatalf("error = %v, want usage error", err)
				}
				return
			}
			if test.wantError {
				if err == nil || errors.Is(err, errUsage) {
					t.Fatalf("error = %v, want confirmation error", err)
				}
				return
			}
			if err != nil || command != test.want {
				t.Fatalf("command/error = %q/%v, want %q/nil", command, err, test.want)
			}
		})
	}
}
