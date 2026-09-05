package main

import (
	"strings"
	"testing"
)

func TestParseCommandSecretUsesStdinAndIsNotEchoed(t *testing.T) {
	secret := "synthetic-secret-value"
	args := []string{"create", "--actor", "deploy", "--code", "widget-a", "--client-id", "11111111-1111-4111-8111-111111111111", "--redirect-uri", "https://example.test/oauth", "--services", "lead-status", "--secret-stdin"}
	command, err := parseCommand(args, strings.NewReader(secret+"\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(command.Secret) != secret || len(command.Services) != 1 {
		t.Fatal("secret or services parsing failed")
	}
	for _, bad := range [][]string{
		{"rotate-secret", "--actor", "deploy", "--code", "widget-a", "--secret", secret},
		{"disable", "--actor", "deploy", "--code", "widget-a", "--client-id", secret},
		{"set-service", "--actor", "deploy", "--code", "widget-a", "--service", "lead-status"},
		{"create", "--actor", "deploy", "--code", "widget-a", "--secret-stdin"},
	} {
		_, err := parseCommand(bad, strings.NewReader(secret))
		if err == nil {
			t.Fatal("invalid command accepted")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatal("secret leaked in parse error")
		}
	}
}

func TestParseCommandLimitsSecretAndRequiresExplicitServices(t *testing.T) {
	base := []string{"create", "--actor", "deploy", "--code", "widget-a", "--client-id", "11111111-1111-4111-8111-111111111111", "--redirect-uri", "https://example.test/oauth", "--secret-stdin"}
	if _, err := parseCommand(base, strings.NewReader("secret")); err == nil {
		t.Fatal("omitted services accepted")
	}
	command, err := parseCommand(append(base, "--services", "none"), strings.NewReader("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if command.Services == nil || len(command.Services) != 0 {
		t.Fatal("none did not create explicit empty grants")
	}
	if _, err := parseCommand(append(base, "--services", "none"), strings.NewReader(strings.Repeat("x", 16385))); err == nil {
		t.Fatal("oversized secret accepted")
	}
}
