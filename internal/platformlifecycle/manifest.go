package platformlifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
)

type Manifest struct {
	Version      int       `json:"version"`
	Integrations []Mapping `json:"integrations"`
}

type Mapping struct {
	Integration uuid.UUID `json:"integration_id"`
	ProductCode string    `json:"product_code"`
	Accounts    []string  `json:"amo_account_ids"`
}

var productCode = regexp.MustCompile(`^[a-z0-9]+(?:[-_][a-z0-9]+)*$`)

func ValidProductCode(code string) bool {
	return len(code) <= 63 && productCode.MatchString(code)
}

func DecodeManifest(raw []byte) (Manifest, string, error) {
	var manifest Manifest
	if err := Decode(raw, &manifest); err != nil {
		return manifest, "", err
	}
	if manifest.Version != 1 || manifest.Integrations == nil {
		return manifest, "", errors.New("expected Platform pilot manifest version 1")
	}
	slices.SortFunc(manifest.Integrations, func(a, b Mapping) int { return strings.Compare(a.Integration.String(), b.Integration.String()) })
	for i, mapping := range manifest.Integrations {
		if mapping.Integration == uuid.Nil || !ValidProductCode(mapping.ProductCode) || len(mapping.Accounts) == 0 {
			return manifest, "", errors.New("invalid Platform pilot mapping")
		}
		if i > 0 && manifest.Integrations[i-1].Integration == mapping.Integration {
			return manifest, "", errors.New("duplicate Platform pilot integration")
		}
		slices.Sort(mapping.Accounts)
		for j, rawAccount := range mapping.Accounts {
			if _, err := AccountID(rawAccount); err != nil {
				return manifest, "", err
			}
			if j > 0 && mapping.Accounts[j-1] == rawAccount {
				return manifest, "", errors.New("duplicate Platform pilot account")
			}
		}
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return manifest, "", err
	}
	digest := sha256.Sum256(body)
	return manifest, hex.EncodeToString(digest[:]), nil
}
