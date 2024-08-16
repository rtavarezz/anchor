package server

import (
	"fmt"
	"github.com/AnomalyFi/hypersdk/codec"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ava-labs/avalanchego/ids"
	"reflect"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type ErrorCode int

const (
	UnknownPayload           ErrorCode = -32001 // Payload does not exist / is not available.
	InvalidForkchoiceState   ErrorCode = -38002 // Forkchoice state is invalid / inconsistent.
	InvalidPayloadAttributes ErrorCode = -38003 // Payload attributes are invalid / inconsistent.
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

type Bytes32 [32]byte

func (b *Bytes32) UnmarshalJSON(text []byte) error {
	return hexutil.UnmarshalFixedJSON(reflect.TypeOf(b), text, b[:])
}

func (b *Bytes32) UnmarshalText(text []byte) error {
	return hexutil.UnmarshalFixedText("Bytes32", text, b[:])
}

func (b Bytes32) MarshalText() ([]byte, error) {
	return hexutil.Bytes(b[:]).MarshalText()
}

func (b Bytes32) String() string {
	return hexutil.Encode(b[:])
}

// TerminalString implements log.TerminalStringer, formatting a string for console
// output during logging.
func (b Bytes32) TerminalString() string {
	return fmt.Sprintf("%x..%x", b[:3], b[29:])
}

type Bytes256 [256]byte

func (b *Bytes256) UnmarshalJSON(text []byte) error {
	return hexutil.UnmarshalFixedJSON(reflect.TypeOf(b), text, b[:])
}

func (b *Bytes256) UnmarshalText(text []byte) error {
	return hexutil.UnmarshalFixedText("Bytes32", text, b[:])
}

func (b Bytes256) MarshalText() ([]byte, error) {
	return hexutil.Bytes(b[:]).MarshalText()
}

func (b Bytes256) String() string {
	return hexutil.Encode(b[:])
}

// TerminalString implements log.TerminalStringer, formatting a string for console
// output during logging.
func (b Bytes256) TerminalString() string {
	return fmt.Sprintf("%x..%x", b[:3], b[253:])
}

type Uint64Quantity = hexutil.Uint64

type BytesMax32 []byte

func (b *BytesMax32) UnmarshalJSON(text []byte) error {
	if len(text) > 64+2+2 { // account for delimiter "", and 0x prefix
		return fmt.Errorf("input too long, expected at most 32 hex-encoded, 0x-prefixed, bytes: %x", text)
	}
	return (*hexutil.Bytes)(b).UnmarshalJSON(text)
}

func (b *BytesMax32) UnmarshalText(text []byte) error {
	if len(text) > 64+2 { // account for 0x prefix
		return fmt.Errorf("input too long, expected at most 32 hex-encoded, 0x-prefixed, bytes: %x", text)
	}
	return (*hexutil.Bytes)(b).UnmarshalText(text)
}

func (b BytesMax32) MarshalText() ([]byte, error) {
	return (hexutil.Bytes)(b).MarshalText()
}

func (b BytesMax32) String() string {
	return hexutil.Encode(b)
}

type Uint256Quantity = uint256.Int

type Data = hexutil.Bytes

type ExecutionPayload struct {
	ParentHash    common.Hash     `json:"parentHash"`
	FeeRecipient  common.Address  `json:"feeRecipient"`
	StateRoot     Bytes32         `json:"stateRoot"`
	ReceiptsRoot  Bytes32         `json:"receiptsRoot"`
	LogsBloom     Bytes256        `json:"logsBloom"`
	PrevRandao    Bytes32         `json:"prevRandao"`
	BlockNumber   Uint64Quantity  `json:"blockNumber"`
	GasLimit      Uint64Quantity  `json:"gasLimit"`
	GasUsed       Uint64Quantity  `json:"gasUsed"`
	Timestamp     Uint64Quantity  `json:"timestamp"`
	ExtraData     BytesMax32      `json:"extraData"`
	BaseFeePerGas Uint256Quantity `json:"baseFeePerGas"`
	BlockHash     common.Hash     `json:"blockHash"`
	// Array of transaction objects, each object is a byte list (DATA) representing
	// TransactionType || TransactionPayload or LegacyTransaction as defined in EIP-2718
	Transactions []Data `json:"transactions"`
}

type OPBid struct {
	Value   *uint256.Int      `json:"value"`
	Payload *ExecutionPayload `json:"payload"`
}

func (bid *OPBid) IsEmpty() bool {
	return bid.Value == nil || bid.Payload == nil
}

// What we send to response
type ExecutionPayload2 struct {
	Slot      uint64       `json:"slot"`
	BlockHash common.Hash `json:"blockHash"`
	// Array of transaction objects, each object is a byte list (DATA) representing
	// TransactionType || TransactionPayload or LegacyTransaction as defined in EIP-2718
	Transactions []Data `json:"transactions"`
}

// e.g seq sends request to Anchor
type SEQHeaderRequest struct {
	Slot           uint64 `json:"slot"`
	NumToBTxs      int   `json:"numtobtxs,omitempty"`
	NumRoBChains   int   `json:"numrobchains,omitempty"`
	NumRoBChunkTxs int   `json:"numrobchunktxs,omitempty"`
}

type SEQHeaderResponse struct {
	// nodeID of chunk producing validator.
	Producer ids.NodeID `json:"producer"`
	// block builder address
	PriorityFeeReceiverAddr codec.Address `json:"priorityfeereceiveraddr"`
	// hash of the chunk
	ChunkHash phase0.Hash32 `json:"chunkhash"`

	ToBHash phase0.Hash32 `json:"tobhash"`

	RoBHashes map[string]phase0.Hash32 `json:"robhashes"`
}

// Request from SEQ for the payload
type SEQPayloadRequest struct {
	Slot                   uint64                                     `json:"slot"`
	ToBBlindedBeaconBlock  AnchorSignedBlindedBeaconBlock            `json:"tobblindedbeaconblock"`
	RoBBlindedBeaconBlocks map[string]AnchorSignedBlindedBeaconBlock `json:"robblindedbeaconblocks"`
}
// Send this back to SEQ
type SEQPayloadResponse struct {
	Slot        uint64                        `json:"slot"`
	ToBPayload  ExecutionPayload2            `json:"tobpayload"`
	RoBPayloads map[string]ExecutionPayload2 `json:"robpayloads"`
}

type AnchorSignedBlindedBeaconBlock struct {
	Message   *AnchorBlindedBeaconBlock
	Signature phase0.BLSSignature `ssz-size:"96"`
}

type AnchorBlindedBeaconBlock struct {
	Slot          phase0.Slot
	ProposerIndex phase0.ValidatorIndex
	ParentRoot    phase0.Root `ssz-size:"32"`
	StateRoot     phase0.Root `ssz-size:"32"`
	Body          *AnchorBlindedBeaconBlockBody
}

type AnchorBlindedBeaconBlockBody struct {
	ExecutionPayloadHeader *AnchorExecutionPayloadHeader
}

// recieving payload from SEQ  
type AnchorExecutionPayloadHeader struct {
	FeeRecipient     bellatrix.ExecutionAddress `ssz-size:"20"`
	StateRoot        [32]byte                   `ssz-size:"32"`
	ReceiptsRoot     [32]byte                   `ssz-size:"32"`
	LogsBloom        [256]byte                  `ssz-size:"256"`
	BlockNumber      uint64
	Timestamp        uint64
	BlockHash        phase0.Hash32 `ssz-size:"32"`
	TransactionsRoot phase0.Root   `ssz-size:"32"`
	ChunkDigest 	 phase0.Root   `ssz-size:"32"`
}
