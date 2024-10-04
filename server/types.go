package server

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/flashbots/go-boost-utils/bls"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type ErrorCode int

// Signature should be same as the signature type used in Baton.
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

type ExecHeadersInfo struct {
	// Make signature based off ToBHash + RoBHashes then we use this signature for Baton/Anchor to check against
	ToBHash   *AnchorHeader            `json:"tobhash"`
	RoBHashes map[string]*AnchorHeader `json:"robhashes"`
}

func NewExecHeadersInfo() *ExecHeadersInfo {
	return &ExecHeadersInfo{
		RoBHashes: make(map[string]*AnchorHeader),
	}
}

type ExecPayloadsInfo struct {
	ToBPayload  *ExecutionPayload           `json:"tobpayload"`
	RoBPayloads map[string]ExecutionPayload `json:"robpayloads"`
}

type AnchorGetHeaderResponse struct {
	ExecHeaders ExecHeadersInfo `json:"exec_headers"`
	BlockInfo   AnchorBlockInfo `json:"block_info"`
	ParentHash  ids.ID          `json:"parent_hash"`
	// Exec headers signed by baton's key.
	ExecHeadersSig []byte `json:"exec_headers_sig"`
}

func NewAnchorGetHeaderResponse() *AnchorGetHeaderResponse {
	return &AnchorGetHeaderResponse{
		ExecHeaders: *NewExecHeadersInfo(),
	}
}

func (msg *AnchorGetHeaderResponse) GetExecPayloadsSig() (*bls.Signature, error) {
	signature, err := bls.SignatureFromBytes(msg.ExecHeadersSig)
	if err != nil {
		return nil, errors.New("invalid exec headers sig, err: " + err.Error())
	}
	return signature, nil
}

func (msg *AnchorGetHeaderResponse) SetExecPayloadsSig(sig *bls.Signature) {
	signatureAsBytes := sig.Bytes()
	msg.ExecHeadersSig = signatureAsBytes[:]
}

func (msg *AnchorGetHeaderResponse) IsEmpty() bool {
	return msg.ExecHeaders.ToBHash == nil && len(msg.ExecHeaders.RoBHashes) == 0
}

func (msg *AnchorGetHeaderResponse) ParentHashAsStr() string {
	return ParentHashToStr(msg.ParentHash)
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

type AnchorGetPayloadResponse struct {
	Slot         uint64           `json:"slot"`
	ExecPayloads ExecPayloadsInfo `json:"execpayloads"`

	// ExecPayloadsSig is the execpayloads with Baton's private key. It is verified by Anchor.
	ExecPayloadsSig []byte `json:"execpayloads_sig"`
}

func (msg *AnchorGetPayloadResponse) GetExecPayloadsSig() (*bls.Signature, error) {
	signature, err := bls.SignatureFromBytes(msg.ExecPayloadsSig)
	if err != nil {
		return nil, errors.New("invalid signed headers, err: " + err.Error())
	}
	return signature, nil
}

func (msg *AnchorGetPayloadResponse) SetExecPayloadsSig(sig *bls.Signature) {
	signatureAsBytes := sig.Bytes()
	msg.ExecPayloadsSig = signatureAsBytes[:]
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

func StrToParentHash(hash string) ids.ID {
	var parentHash ids.ID
	copy(parentHash[:], hash)
	return parentHash
}

func ParentHashToStr(parentHash ids.ID) string {
	return string(parentHash[:])
}
