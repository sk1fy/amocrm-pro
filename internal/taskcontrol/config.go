package taskcontrol

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
)

type Config struct {
	AssertionKeyID string
	AssertionKey   ed25519.PrivateKey
	Address        string
	Keys           map[string]string
}

func LoadConfig() (Config, error) {
	c := Config{Address: os.Getenv("TASK_CONTROL_HTTP_ADDRESS")}
	raw := os.Getenv("TASK_CONTROL_SERVICE_KEYS")
	if c.Address == "" && raw == "" {
		return c, nil
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return c, errors.New("invalid TASK_CONTROL_HTTP_ADDRESS")
	}
	if os.Getenv("TASK_CONTROL_TLS_PROXY") != "true" {
		return c, errors.New("task control requires private TLS proxy")
	}
	if json.Unmarshal([]byte(raw), &c.Keys) != nil || len(c.Keys) == 0 {
		return c, errors.New("invalid TASK_CONTROL_SERVICE_KEYS")
	}
	for k, v := range c.Keys {
		if k == "" || len(k) > 64 || len(v) < 32 {
			return c, errors.New("invalid task control service key")
		}
	}
	c.AssertionKeyID = os.Getenv("TASK_CONTROL_ASSERTION_KEY_ID")
	rawKey, err := base64.StdEncoding.DecodeString(os.Getenv("TASK_CONTROL_ASSERTION_PRIVATE_KEY"))
	if err != nil || len(rawKey) != ed25519.PrivateKeySize || (c.AssertionKeyID == "" || len(c.AssertionKeyID) > 64 || strings.ContainsAny(c.AssertionKeyID, ".\n")) {
		return c, errors.New("invalid task control assertion key")
	}
	c.AssertionKey = ed25519.PrivateKey(rawKey)
	return c, nil
}
