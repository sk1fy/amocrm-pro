package buildinfo

import (
	"bytes"
	"testing"
)

func TestVersionDoesNotRequireRuntimeConfiguration(t *testing.T) {
	old := Revision
	t.Cleanup(func() { Revision = old })
	Revision = "abc-dirty"
	var out bytes.Buffer
	if !PrintVersion([]string{"activity", "--version"}, &out) || out.String() != "abc-dirty\n" {
		t.Fatalf("%q", out.String())
	}
	if PrintVersion([]string{"activity", "up"}, &out) {
		t.Fatal("intercepted runtime command")
	}
}
