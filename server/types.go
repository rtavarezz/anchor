package server

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
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
	// SEQ marshalled txs
	Transactions []byte `json:"transactions"`
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
	// hypersdk transactions in byte slice format
	Transactions []byte `json:"transactions"`
}

func NewExecutionPayload() ExecutionPayload {
	return ExecutionPayload{
		Transactions: make([]byte, 0),
	}
}

type AnchorHeader struct {
	Header    *common.Hash `json:"header"`
	BlockHash string       `json:"block_hash"`
	Value     *big.Int     `json:"value"`
}

func NewAnchorGetHeaderResponse() *AnchorGetHeaderResponse {
	return &AnchorGetHeaderResponse{
		ExecHeaders: *NewExecPayloadsInfo(),
	}
}

func (r *AnchorGetHeaderResponse) GetExecPayloadsSig() (*bls.Signature, error) {
	signature, err := bls.SignatureFromBytes(r.ExecHeadersSig)
	if err != nil {
		return nil, errors.New("invalid exec headers sig, err: " + err.Error())
	}
	return signature, nil
}

func (r *AnchorGetHeaderResponse) SetExecPayloadsSig(sig *bls.Signature) {
	signatureAsBytes := sig.Bytes()
	r.ExecHeadersSig = signatureAsBytes[:]
}

func (msg *AnchorGetHeaderResponse) IsEmpty() bool {
	return msg.ExecHeaders.ToBHash == nil && len(msg.ExecHeaders.RoBHashes) == 0
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

type AnchorGetHeaderResponse struct {
	ExecHeaders ExecHeadersInfo `json:"exec_headers"`
	BlockInfo   AnchorBlockInfo `json:"block_info"`
	ParentHash  common.Hash     `json:"parent_hash"`
	HeadersHash common.Hash     `json:"headers_hash"`
	// Exec headers signed by baton's key.
	ExecHeadersSig []byte `json:"exec_headers_sig"`
}

type AnchorBlockInfo struct {
	Slot uint64 `json:"slot"`
	// nodeID of chunk producing validator.
	Producer       ids.NodeID    `json:"producer"`
	ProposerPubkey bls.PublicKey `json:"proposer_pubkey"`
}

type AnchorGetPayloadRequest struct {
	Slot           uint64 `json:"slot"`
	ProposerPubKey []byte `json:"proposer_pubkey"`
	ParentHash     string `json:"parent_hash"`
	// Exec headers signed by validator's private key. Should be [48]byte signature.
	SignedHeaders []byte `json:"signed_headers"`
}

func (r *AnchorGetPayloadRequest) GetSignedHeaders() (*bls.Signature, error) {
	signature, err := bls.SignatureFromBytes(r.SignedHeaders)
	if err != nil {
		return nil, errors.New("invalid signed headers, err: " + err.Error())
	}
	return signature, nil
}

type ExecPayloadsInfo struct {
	ToBPayload  *ExecutionPayload           `json:"tobpayload"`
	RoBPayloads map[string]ExecutionPayload `json:"robpayloads"`
}

// Note ExecPayloadsSig is the execpayloads with Baton's private key. It is verified by Anchor.
type AnchorGetPayloadResponse struct {
	Slot            uint64           `json:"slot"`
	ExecPayloads    ExecPayloadsInfo `json:"execpayloads"`
	ExecPayloadsSig []byte           `json:"execpayloads_sig"`
}

func (r *AnchorGetPayloadResponse) GetExecPayloadsSig() (*bls.Signature, error) {
	signature, err := bls.SignatureFromBytes(r.ExecPayloadsSig)
	if err != nil {
		return nil, errors.New("invalid signed headers, err: " + err.Error())
	}
	return signature, nil
}

func (r *AnchorGetPayloadResponse) SetExecPayloadsSig(sig *bls.Signature) {
	signatureAsBytes := sig.Bytes()
	r.ExecPayloadsSig = signatureAsBytes[:]
}

func (msg *AnchorGetPayloadResponse) IsEmpty() bool {
	return msg.ExecPayloads.ToBPayload == nil && len(msg.ExecPayloads.RoBPayloads) == 0
}

func (msg *AnchorGetPayloadResponse) HasToBTxs() bool {
	return msg.ExecPayloads.ToBPayload != nil
}

func (msg *AnchorGetPayloadResponse) NumRoBChains() int {
	return len(msg.ExecPayloads.RoBPayloads)
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

// VerifyHeaderSignature verifies that the getHeader ExecHeaders have been signed with the given public key
func VerifyHeaderSignature(response *AnchorGetHeaderResponse, pubKey bls.PublicKey) (bool, error) {
	payloadHash, err := hashExecHeaders(&response.ExecHeaders)
	if err != nil {
		return false, err
	}

	payloadSignatureBytes := response.ExecHeadersSig
	pubKeyBytes := pubKey.Bytes()

	return bls.VerifySignatureBytes(payloadHash[:], payloadSignatureBytes[:], pubKeyBytes[:])
}

// VerifyPayloadSignature verifies that the getHeader ExecHeaders have been signed with the given public key
func VerifyPayloadSignature(response *AnchorGetPayloadResponse, pubKey bls.PublicKey) (bool, error) {
	payloadHash, err := hashExecPayloads(&response.ExecPayloads)
	if err != nil {
		return false, err
	}

	payloadSignatureBytes := response.ExecPayloadsSig
	pubKeyBytes := pubKey.Bytes()

	return bls.VerifySignatureBytes(payloadHash[:], payloadSignatureBytes[:], pubKeyBytes[:])
}

func GetExecHeaderSignature(headers *ExecHeadersInfo, secretKey *bls.SecretKey) (*bls.Signature, error) {
	// Step 1: Hash the ExecHeaders (ToBHash + RoBHashes) data
	payloadHash, err := hashExecHeaders(headers)
	if err != nil {
		return nil, err
	}

	// Step 2: Sign the hashed headers using the secret key
	signature := bls.Sign(secretKey, payloadHash[:])
	return signature, nil
}

func GetExecPayloadSignature(payloads *ExecPayloadsInfo, secretKey *bls.SecretKey) (*bls.Signature, error) {
	// Step 1: Hash the ExecHeaders (ToBHash + RoBHashes) data
	payloadHash, err := hashExecPayloads(payloads)
	if err != nil {
		return nil, err
	}

	// Step 2: Sign the hashed payloads using the secret key
	signature := bls.Sign(secretKey, payloadHash[:])
	return signature, nil
}

func SignAnchorGetHeaderResponse(response *AnchorGetHeaderResponse, secretKey *bls.SecretKey) error {
	signature, err := GetExecHeaderSignature(&response.ExecHeaders, secretKey)
	if err != nil {
		return errors.New("failed to sign anchor header response, err: " + err.Error())
	}

	response.SetExecPayloadsSig(signature)
	return nil
}

func SignAnchorGetPayloadResponse(response *AnchorGetPayloadResponse, secretKey *bls.SecretKey) error {
	signature, err := GetExecPayloadSignature(&response.ExecPayloads, secretKey)
	if err != nil {
		return errors.New("failed to sign anchor header response, err: " + err.Error())
	}

	response.SetExecPayloadsSig(signature)
	return nil
}

func hashExecHeaders(headers *ExecHeadersInfo) ([32]byte, error) {
	// Use JSON serialization to hash the struct
	payloadBytes, err := json.Marshal(*headers)
	if err != nil {
		return [32]byte{}, fmt.Errorf("failed to serialize ExecHeaders: %w", err)
	}

	// Use sha256 to hash the serialized ExecHeaders data
	hash := sha256.Sum256(payloadBytes)
	return hash, nil
}

func hashExecPayloads(payloads *ExecPayloadsInfo) ([32]byte, error) {
	// Use JSON serialization to hash the struct
	payloadBytes, err := json.Marshal(*payloads)
	if err != nil {
		return [32]byte{}, fmt.Errorf("failed to serialize ExecHeaders: %w", err)
	}

	// Use sha256 to hash the serialized ExecHeaders data
	hash := sha256.Sum256(payloadBytes)
	return hash, nil
}
