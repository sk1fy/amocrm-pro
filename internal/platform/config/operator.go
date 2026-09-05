package config

// LoadOperator shares encryption and environment safety checks with API/worker
// without loading bootstrap credentials or requiring any public HTTP listener.
func LoadOperator() (Common, error) {
	return loadCommon("amocrm-integrations", ":8080")
}
