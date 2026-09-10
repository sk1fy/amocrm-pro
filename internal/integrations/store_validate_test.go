package integrations

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCommandValidateInstallationActions(t *testing.T) {
	id := uuid.New()
	valid := Command{Action: "uninstall", Actor: "operator", Code: "widget-a", InstallationID: id}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, command := range []Command{
		{Action: "uninstall", Actor: "operator", Code: "widget-a"},
		{Action: "disable", Actor: "operator", Code: "widget-a", InstallationID: id},
		{Action: "uninstall", Actor: "operator", Code: "widget-a", InstallationID: id, Secret: []byte("secret")},
		{Action: "not-a-command", Actor: "operator", Code: "widget-a", InstallationID: id},
	} {
		if err := command.Validate(); err == nil {
			t.Fatalf("accepted %#v", command)
		}
	}
	if err := (Command{Action: "disable-installation", Actor: "operator", Code: "widget-a", InstallationID: id}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Command{Action: "revoke", Actor: "operator", Code: "widget-a", InstallationID: id}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallationTransitionRejectsUninstalledEnable(t *testing.T) {
	if _, err := installationTransition("enable-installation", "uninstalled"); err == nil {
		t.Fatal("enable of uninstalled accepted")
	}
	if _, err := installationTransition("disable-installation", "uninstalled"); err == nil {
		t.Fatal("disable of uninstalled accepted")
	}
	if _, err := installationTransition("revoke", "uninstalled"); err == nil {
		t.Fatal("revoke of uninstalled accepted")
	}
	next, err := installationTransition("uninstall", "uninstalled")
	if err != nil || next != "uninstalled" {
		t.Fatalf("idempotent uninstall: %q %v", next, err)
	}
	if !strings.Contains(mustTransitionError(t, "enable-installation", "reauth_required"), "disabled") {
		t.Fatal("enable from reauth_required should require disabled")
	}
}

func mustTransitionError(t *testing.T, action, current string) string {
	t.Helper()
	_, err := installationTransition(action, current)
	if err == nil {
		t.Fatalf("%s from %s accepted", action, current)
	}
	return err.Error()
}
