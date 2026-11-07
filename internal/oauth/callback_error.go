package oauth

import "errors"

type callbackOutcome uint8

const (
	callbackUncertain callbackOutcome = iota
	callbackRepeatable
	callbackConsumed
)

type callbackError struct {
	outcome callbackOutcome
	cause   error
}

func (e *callbackError) Error() string { return e.cause.Error() }
func (e *callbackError) Unwrap() error { return e.cause }

func withCallbackOutcome(err error, outcome callbackOutcome) error {
	if err == nil {
		return nil
	}
	return &callbackError{outcome: outcome, cause: err}
}

func callbackOutcomeOf(err error) callbackOutcome {
	var failure *callbackError
	if errors.As(err, &failure) {
		return failure.outcome
	}
	if errors.Is(err, errInstallCapacity) {
		return callbackRepeatable
	}
	return callbackUncertain
}
