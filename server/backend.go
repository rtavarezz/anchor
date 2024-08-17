package server

const (
	// Router paths
	pathStatus            = "/eth/v1/builder/status"
	pathRegisterValidator = "/eth/v1/builder/validators"
	pathGetHeader         = "/eth/v1/builder/header/{slot:[0-9]+}/{parent_hash:0x[a-fA-F0-9]+}/{pubkey:0x[a-fA-F0-9]+}"
	pathGetPayload        = "/eth/v1/builder/blinded_blocks"
	pathGetOPPayload      = "/eth/v1/builder/get_payload/{parent_hash:0x[a-fA-F0-9]+}"
	pathGetHeader2        = "/eth/v1/builder/header2/{slot:[0-9]+}/{parent_hash:0x[a-fA-F0-9]+}/{pubkey:0x[a-fA-F0-9]+}/{numtobtxs:[0-9]+}/{numrobchains:[0-9]+}/{numrobchunktxs:[0-9]+}"
	pathGetPayload2       = "/eth/v1/builder/blinded_blocks2"
	pathGetOPPayload2     = "/eth/v1/builder/get_payload2/{parent_hash:0x[a-fA-F0-9]+}"

	// // Relay Monitor paths
	// pathAuctionTranscript = "/monitor/v1/transcript"
)

// NumToBTxs      int    `json:"numtobtxs,omitempty"`
// 	NumRoBChains   int    `json:"numrobchains,omitempty"`
// 	NumRoBChunkTxs int    `json:"numrobchunktxs,omitempty"`