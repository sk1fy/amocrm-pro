package serviceapi

// Operation states are shared by the application, gRPC and Activity JSON
// contracts. Queue/storage statuses (including Core jobs) are separate types.
const (
	OperationAccepted  = "accepted"
	OperationRunning   = "running"
	OperationRetry     = "retry"
	OperationPaused    = "paused"
	OperationFailed    = "failed"
	OperationSucceeded = "succeeded"
)

// CanonicalOperation translates historical owner storage at its application
// boundary. Keep persisted "completed" readable without a database migration;
// never expose it as a second success spelling or rewrite existing Core jobs.
func CanonicalOperation(operation Operation) (Operation, error) {
	if operation.State == "completed" {
		operation.State = OperationSucceeded
	}
	switch operation.State {
	case OperationAccepted, OperationRunning, OperationRetry, OperationPaused, OperationFailed, OperationSucceeded:
		return operation, nil
	default:
		return Operation{}, Fail(Internal, "owner returned an unsupported operation state")
	}
}
