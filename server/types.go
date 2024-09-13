package server

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/AnomalyFi/hypersdk/codec"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/flashbots/go-boost-utils/bls"
	"math/big"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type ErrorCode int

// TODO: Resolve what should be done with the below later.
// This should be same as the signature type used in Baton.
// This is included here because the Signature type was modified in the latest version of flashbots.
type Signature [96]byte

const (
	TestPrivateKeyValue = "77619a19a837f894fa5c90e58ee3e3d69e382936d323d987bbde923da92a5ac5"
	TestAddressValue    = "0x59131f2c045f70Be0dDA50D86b6ED2b18C5012cf"
)

// InputError distinguishes an user-input error from regular rpc errors,
// to help the (Engine) API user divert from accidental input mistakes.
type InputError struct {
	Inner error
	Code  ErrorCode
}

func (ie InputError) Error() string {
	return fmt.Sprintf("input error %d: %s", ie.Code, ie.Inner.Error())
}

func (ie InputError) Unwrap() error {
	return ie.Inner
}

// Is checks if the error is the given target type.
// Any type of InputError counts, regardless of code.
func (ie InputError) Is(target error) bool {
	_, ok := target.(InputError)
	return ok // we implement Unwrap, so we do not have to check the inner type now
}

type Data = hexutil.Bytes

type OPBid struct {
	Value   *uint256.Int      `json:"value"`
	Payload *ExecutionPayload `json:"payload"`
}

func (bid *OPBid) IsEmpty() bool {
	return bid.Value == nil || bid.Payload == nil
}

// What we send to response
type ExecutionPayload2 struct {
	Slot      uint64      `json:"slot"`
	BlockHash common.Hash `json:"blockHash"`
	// Array of transaction objects, each object is a byte list (DATA) representing
	// TransactionType || TransactionPayload or LegacyTransaction as defined in EIP-2718
	Transactions []Data `json:"transactions"`
}

// e.g seq sends request to Anchor
type SEQHeaderRequest struct {
	Slot           uint64 `json:"slot"`
	NumToBTxs      int    `json:"numtobtxs,omitempty"`
	NumRoBChains   int    `json:"numrobchains,omitempty"`
	NumRoBChunkTxs int    `json:"numrobchunktxs,omitempty"`
}

type SEQHeaderResponse struct {
	Slot uint64 `json:"slot"`
	// nodeID of chunk producing validator.
	Producer ids.NodeID `json:"producer"`
	// block builder address
	PriorityFeeReceiverAddr codec.Address `json:"priorityfeereceiveraddr"`
	// hash of the anchor chunks (tob + robs)
	ChunkHash common.Hash            `json:"chunkhash"`
	ToBHash   *common.Hash           `json:"tobhash"`
	RoBHashes map[string]common.Hash `json:"robhashes"`
}

func NewSEQHeaderResponse(slot uint64) SEQHeaderResponse {
	return SEQHeaderResponse{
		Slot:      slot,
		RoBHashes: make(map[string]common.Hash),
	}
}

func (msg *SEQHeaderResponse) IsEmpty() bool {
	return msg.ToBHash == nil && (msg.RoBHashes == nil || len(msg.RoBHashes) == 0)
}

// @TODO: This is deprecated. Remove later when mock methods no longer needed.
type SEQPayloadRequest struct {
	Slot                   uint64                                    `json:"slot"`
	ToBBlindedBeaconBlock  AnchorSignedBlindedBeaconBlock            `json:"tobblindedbeaconblock"`
	RoBBlindedBeaconBlocks map[string]AnchorSignedBlindedBeaconBlock `json:"robblindedbeaconblocks"`
}

// @TODO: This is deprecated. Remove later when mock methods no longer needed.
func NewSEQPayloadRequest(slot uint64) SEQPayloadRequest {
	return SEQPayloadRequest{
		Slot:                   slot,
		RoBBlindedBeaconBlocks: make(map[string]AnchorSignedBlindedBeaconBlock),
	}
}

// Send this back to SEQ
type SEQPayloadResponse struct {
	Slot        uint64                       `json:"slot"`
	ToBPayload  ExecutionPayload2            `json:"tobpayload"`
	RoBPayloads map[string]ExecutionPayload2 `json:"robpayloads"`
}

func NewSEQPayloadResponse(slot uint64) SEQPayloadResponse {
	return SEQPayloadResponse{
		Slot:        slot,
		RoBPayloads: make(map[string]ExecutionPayload2),
	}
}

// @TODO: This is deprecated. Remove later when mock methods no longer needed.
type AnchorSignedBlindedBeaconBlock struct {
	Message   *AnchorBlindedBeaconBlock
	Signature phase0.BLSSignature `ssz-size:"96"`
}

// @TODO: This is deprecated. Remove later when mock methods no longer needed.
type AnchorBlindedBeaconBlock struct {
	Slot          phase0.Slot
	ProposerIndex phase0.ValidatorIndex
	ParentRoot    phase0.Root `ssz-size:"32"`
	StateRoot     phase0.Root `ssz-size:"32"`
	Body          *AnchorBlindedBeaconBlockBody
}

// @TODO: This is deprecated. Remove later when mock methods no longer needed.
type AnchorBlindedBeaconBlockBody struct {
	ExecutionPayloadHeader *AnchorExecutionPayloadHeader
}

// @TODO: This is deprecated. Remove later when mock methods no longer needed.
// receiving payload from SEQ
type AnchorExecutionPayloadHeader struct {
	FeeRecipient     bellatrix.ExecutionAddress `ssz-size:"20"`
	StateRoot        [32]byte                   `ssz-size:"32"`
	ReceiptsRoot     [32]byte                   `ssz-size:"32"`
	LogsBloom        [256]byte                  `ssz-size:"256"`
	BlockNumber      uint64
	Timestamp        uint64
	BlockHash        phase0.Hash32 `ssz-size:"32"`
	TransactionsRoot phase0.Root   `ssz-size:"32"`
	ChunkDigest      phase0.Root   `ssz-size:"32"`
}

func (r *SEQPayloadRequest) ToJSON() ([]byte, error) {
	return json.Marshal(r)
}

// SEQHeaderRequest Deserialization
func (r *SEQPayloadRequest) FromJSON(data []byte) error {
	return json.Unmarshal(data, r)
}

// SEQHeaderResponse Serialization
func (r *SEQPayloadResponse) ToJSON() ([]byte, error) {
	return json.Marshal(r)
}

// SEQHeaderResponse Deserialization
func (r *SEQPayloadResponse) FromJSON(data []byte) error {
	return json.Unmarshal(data, r)
}

func (r *SEQHeaderRequest) ToJSON() ([]byte, error) {
	return json.Marshal(r)
}

// SEQHeaderResponse Deserialization
func (r *SEQHeaderResponse) FromJSON(data []byte) error {
	return json.Unmarshal(data, r)
}

type ExecutionPayload struct {
	// Array of transaction objects, each object is a byte list (DATA) representing
	// TransactionType || TransactionPayload or LegacyTransaction as defined in EIP-2718
	Transactions []hexutil.Bytes `json:"transactions"`
}

func NewExecutionPayload() ExecutionPayload {
	return ExecutionPayload{
		Transactions: make([]hexutil.Bytes, 0),
	}
}

type AnchorHeader struct {
	Header    *common.Hash `json:"header"`
	BlockHash string       `json:"block_hash"`
	Value     *big.Int     `json:"value"`
}

type AnchorGetHeaderResponse struct {
	ExecPayloads    ExecHeadersInfo `json:"exec_payloads"`
	BlockInfo       AnchorBlockInfo `json:"block_info"`
	ExecPayloadsSig bls.Signature   `json:"exec_payloads_sig"` // signature used for signing the bid
}

func NewAnchorGetHeaderResponse() *AnchorGetHeaderResponse {
	return &AnchorGetHeaderResponse{
		ExecPayloads: *NewExecPayloadsInfo(),
	}
}

func (msg *AnchorGetHeaderResponse) IsEmpty() bool {
	return msg.ExecPayloads.ToBHash == nil && len(msg.ExecPayloads.RoBHashes) == 0
}

type ExecHeadersInfo struct {
	// Make signature based off ToBHash + RoBHashes then we use this signature for Baton/Anchor to check against
	ToBHash   *AnchorHeader            `json:"tobhash"`
	RoBHashes map[string]*AnchorHeader `json:"robhashes"`
}

func NewExecPayloadsInfo() *ExecHeadersInfo {
	return &ExecHeadersInfo{
		RoBHashes: make(map[string]*AnchorHeader),
	}
}

type AnchorBlockInfo struct {
	// note: Message should be the anchor req
	Slot uint64 `json:"slot"`
	// nodeID of chunk producing validator.
	Producer ids.NodeID `json:"producer"`
	// hash of the anchor chunks (tob + robs)
	ChunkHash      common.Hash   `json:"chunkhash"`
	ProposerPubkey bls.PublicKey `json:"proposer_pubkey"`
}

type AnchorGetPayloadRequest struct {
	Slot          uint64        `json:"slot"`
	ProposerIndex uint64        `json:"proposer_index"`
	BlockHash     string        `json:"block_hash"`
	SignedHeaders bls.Signature `json:"signed_headers"`
}

type ExecPayloadsInfo struct {
	ToBPayload  *ExecutionPayload           `json:"tobpayload"`
	RoBPayloads map[string]ExecutionPayload `json:"robpayloads"`
}

type AnchorGetPayloadResponse struct {
	Slot            uint64           `json:"slot"`
	ExecPayloads    ExecPayloadsInfo `json:"execpayloads"`
	ExecPayloadsSig bls.Signature    `json:"execpayloads_sig"`
}

func (msg *AnchorGetPayloadResponse) IsEmpty() bool {
	return msg.ExecPayloads.ToBPayload == nil && len(msg.ExecPayloads.RoBPayloads) == 0
}

func (msg *AnchorGetPayloadResponse) NumToBTxs() int {
	if msg.ExecPayloads.ToBPayload == nil {
		return 0
	}
	return len(msg.ExecPayloads.ToBPayload.Transactions)
}

func (msg *AnchorGetPayloadResponse) NumRoBTxs() int {
	var numTxs int
	for _, txs := range msg.ExecPayloads.RoBPayloads {
		numTxs = numTxs + len(txs.Transactions)
	}
	return numTxs
}

func NewAnchorGetPayloadResponse(slot uint64, needsToB bool) AnchorGetPayloadResponse {
	var tob *ExecutionPayload
	if needsToB {
		payload := NewExecutionPayload()
		tob = &payload
	}

	execPayloads := ExecPayloadsInfo{
		ToBPayload:  tob,
		RoBPayloads: make(map[string]ExecutionPayload),
	}

	return AnchorGetPayloadResponse{
		Slot:         slot,
		ExecPayloads: execPayloads,
	}
}

// VerifySignature verifies that the getHeader ExecPayloads have been signed with the given public key
func VerifyHeaderSignatures2(response *AnchorGetHeaderResponse, pubKey bls.PublicKey) (bool, error) {
	payloadHash, err := hashExecPayloads(&response.ExecPayloads)
	if err != nil {
		return false, err
	}

	execPayloadsBytes := response.ExecPayloadsSig.Bytes()
	pubKeyBytes := pubKey.Bytes()

	return bls.VerifySignatureBytes(payloadHash[:], execPayloadsBytes[:], pubKeyBytes[:])
}

// VerifySignature verifies that the getHeader ExecPayloads have been signed with the given public key
func VerifyHeaderSignatures(response *AnchorGetHeaderResponse, pubKey bls.PublicKey) (bool, error) {
	payloadHash, err := hashExecPayloads(&response.ExecPayloads)
	if err != nil {
		return false, err
	}

	payloadSignatureBytes := response.ExecPayloadsSig.Bytes()
	pubKeyBytes := pubKey.Bytes()

	return bls.VerifySignatureBytes(payloadHash[:], payloadSignatureBytes[:], pubKeyBytes[:])
}

func SignExecPayloads(payload *ExecHeadersInfo, secretKey *bls.SecretKey) (*bls.Signature, error) {
	// Step 1: Hash the ExecPayloads (ToBHash + RoBHashes) data
	payloadHash, err := hashExecPayloads(payload)
	if err != nil {
		return nil, err
	}

	// Step 2: Sign the hashed payload using the secret key
	signature := bls.Sign(secretKey, payloadHash[:])
	return signature, nil
}

func hashExecPayloads(payload *ExecHeadersInfo) ([32]byte, error) {
	// Use JSON serialization to hash the struct
	payloadBytes, err := json.Marshal(*payload)
	if err != nil {
		return [32]byte{}, fmt.Errorf("failed to serialize ExecPayloads: %w", err)
	}

	// Use sha256 to hash the serialized ExecPayloads data
	hash := sha256.Sum256(payloadBytes)
	return hash, nil
}
