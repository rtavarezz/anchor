package server

const (
	// Router paths
	pathStatus            = "/eth/v1/builder/status"
	pathRegisterValidator = "/eth/v1/builder/validators"
	pathGetHeader         = "/eth/v1/builder/header/{slot}/{parent_hash}/{pubkey}"
	pathGetPayload        = "/eth/v1/builder/blinded_blocks"
)
