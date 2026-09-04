package widgetauth

import "errors"

// Stable, safe-to-log reason codes for widget-auth rejections. Values must
// never contain token bytes, raw payloads, signatures, or secrets.
const (
	reasonHeaderMissing        = "auth_header_missing"
	reasonHeadersConflict      = "auth_headers_conflict"
	reasonXAuthTokenInvalid    = "auth_x_auth_token_invalid"
	reasonAuthorizationInvalid = "auth_authorization_invalid"

	reasonTokenFormat         = "token_format_invalid"
	reasonTenantNotFound      = "tenant_not_found"
	reasonMaterialMismatch    = "material_mismatch"
	reasonSignatureInvalid    = "signature_invalid"
	reasonPayloadShapeInvalid = "payload_shape_invalid"
	reasonClaimsInvalid       = "claims_invalid"
	reasonReplay              = "replay"

	reasonTenantLookupError   = "tenant_lookup_error"
	reasonDeriveAudienceError = "derive_audience_error"
	reasonDeriveIssuerError   = "derive_issuer_error"
	reasonDecryptSecretError  = "decrypt_secret_error"
	reasonInternal            = "internal"
)

// Failure carries a stable reason code and optional safe diagnostic facts.
// It intentionally never stores token bytes, payloads, signatures, or secrets.
type Failure struct {
	Reason string
	Err    error

	ClientUUIDMatch  *bool
	AccountIDMatch   *bool
	UserIDValid      *bool
	AudienceMatch    *bool
	IssuerMatch      *bool
	TokenLifetimeSec *float64
	MaxLifetimeSec   *float64
	LeewaySec        *float64
}

func (f *Failure) Error() string { return f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

func newFailure(reason string, err error) *Failure {
	return &Failure{Reason: reason, Err: err}
}

func failureReason(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Reason
	}
	switch {
	case errors.Is(err, ErrReplay):
		return reasonReplay
	case errors.Is(err, ErrInvalidToken):
		return reasonTokenFormat
	default:
		return reasonInternal
	}
}

func (f *Failure) logAttrs() []any {
	attrs := make([]any, 0, 16)
	if f.ClientUUIDMatch != nil {
		attrs = append(attrs, "client_uuid_match", *f.ClientUUIDMatch)
	}
	if f.AccountIDMatch != nil {
		attrs = append(attrs, "account_id_match", *f.AccountIDMatch)
	}
	if f.UserIDValid != nil {
		attrs = append(attrs, "user_id_valid", *f.UserIDValid)
	}
	if f.AudienceMatch != nil {
		attrs = append(attrs, "aud_match", *f.AudienceMatch)
	}
	if f.IssuerMatch != nil {
		attrs = append(attrs, "iss_match", *f.IssuerMatch)
	}
	if f.TokenLifetimeSec != nil {
		attrs = append(attrs, "token_lifetime_sec", *f.TokenLifetimeSec)
	}
	if f.MaxLifetimeSec != nil {
		attrs = append(attrs, "max_lifetime_sec", *f.MaxLifetimeSec)
	}
	if f.LeewaySec != nil {
		attrs = append(attrs, "leeway_sec", *f.LeewaySec)
	}
	return attrs
}

func boolPtr(b bool) *bool          { return &b }
func float64Ptr(f float64) *float64 { return &f }
