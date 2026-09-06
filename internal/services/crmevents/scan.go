package crmevents

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func canonicalEvent(e serviceapi.Event) (serviceapi.Event, []byte, error) {
	// JSON object key order and whitespace are not event mutations.
	canonical := func(b json.RawMessage) (json.RawMessage, error) {
		if len(b) == 0 {
			return json.RawMessage("[]"), nil
		}
		var v any
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		return json.Marshal(v)
	}
	var err error
	e.ValueBefore, err = canonical(e.ValueBefore)
	if err != nil {
		return e, nil, err
	}
	e.ValueAfter, err = canonical(e.ValueAfter)
	if err != nil {
		return e, nil, err
	}
	b, err := json.Marshal(e)
	return e, b, err
}
func pageDigest(previous string, events []serviceapi.Event) (string, error) {
	h := sha256.New()
	_, _ = h.Write([]byte(previous))
	for _, e := range events {
		_, b, err := canonicalEvent(e)
		if err != nil {
			return "", err
		}
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
