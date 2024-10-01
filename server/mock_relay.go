package server

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ethereum/go-ethereum/common"
	"golang.org/x/exp/rand"

	builderApiCapella "github.com/attestantio/go-builder-client/api/capella"
	builderApiDeneb "github.com/attestantio/go-builder-client/api/deneb"
	builderApiV1 "github.com/attestantio/go-builder-client/api/v1"
	builderSpec "github.com/attestantio/go-builder-client/spec"
	"github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/capella"
	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/flashbots/go-boost-utils/bls"
	"github.com/flashbots/go-boost-utils/ssz"
	"github.com/gorilla/mux"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
)

const (
	mockRelaySecretKeyHex = "0x4e343a647c5a5c44d76c2c58b63f02cdf3a9a0ec40f102ebc26363b4b1b95033"
)

var (
	skBytes, _                = hexutil.Decode(mockRelaySecretKeyHex)
	mockRelaySecretKey, _     = bls.SecretKeyFromBytes(skBytes)
	mockRelayPublicKey, _     = bls.PublicKeyFromSecretKey(mockRelaySecretKey)
	TestMockExpectedToBValue  = big.NewInt(20000)
	TestMockExpectedRoBValue1 = big.NewInt(20100)
	TestMockExpectedRoBValue2 = big.NewInt(20002)
	TestChainID1              = "chain_id1"
	TestChainID2              = "chain_id2"
)

// mockRelay is used to fake a relay's behavior.
// You can override each of its handler by setting the instance's HandlerOverride_METHOD_TO_OVERRIDE to your own
// handler.
type mockRelay struct {
	// Used to panic if impossible error happens
	t *testing.T

	// KeyPair used to sign messages
	secretKey  *bls.SecretKey
	publicKey  *bls.PublicKey
	RelayEntry RelayEntry

	// Used to count each Request made to the relay, either if it fails or not, for each method
	mu           sync.Mutex
	requestCount map[string]int

	// Overriders
	handlerOverrideRegisterValidator func(w http.ResponseWriter, req *http.Request)
	handlerOverrideGetHeader         func(w http.ResponseWriter, req *http.Request)
	handlerOverrideGetPayload        func(w http.ResponseWriter, req *http.Request)

	// Default responses placeholders, used if overrider does not exist
	GetHeaderResponse  *AnchorGetHeaderResponse
	GetPayloadResponse *AnchorGetPayloadResponse

	// Server section
	Server        *httptest.Server
	ResponseDelay time.Duration
}

// newMockRelay creates a mocked relay which implements the backend.BoostBackend interface
// A secret key must be provided to sign default and custom response messages
func newMockRelay(t *testing.T) *mockRelay {
	t.Helper()
	relay := &mockRelay{t: t, secretKey: mockRelaySecretKey, publicKey: mockRelayPublicKey, requestCount: make(map[string]int)}

	// Initialize server
	relay.Server = httptest.NewServer(relay.getRouter())

	// Create the RelayEntry with correct pubkey
	url, err := url.Parse(relay.Server.URL)
	require.NoError(t, err)
	urlWithKey := fmt.Sprintf("%s://%s@%s", url.Scheme, hexutil.Encode(bls.PublicKeyToBytes(mockRelayPublicKey)), url.Host)
	relay.RelayEntry, err = NewRelayEntry(urlWithKey)
	require.NoError(t, err)
	return relay
}

// newTestMiddleware creates a middleware which increases the Request counter and creates a fake delay for the response
func (m *mockRelay) newTestMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			// Request counter
			m.mu.Lock()
			url := r.URL.EscapedPath()
			m.requestCount[url]++
			m.mu.Unlock()

			// Artificial Delay
			if m.ResponseDelay > 0 {
				time.Sleep(m.ResponseDelay)
			}

			next.ServeHTTP(w, r)
		},
	)
}

// getRouter registers all methods from the backend, apply the test middleware and return the configured router
func (m *mockRelay) getRouter() http.Handler {
	// Create router.
	r := mux.NewRouter()

	// Register handlers
	r.HandleFunc("/", m.handleRoot).Methods(http.MethodGet)
	r.HandleFunc(pathStatus, m.handleStatus).Methods(http.MethodGet)
	r.HandleFunc(pathRegisterValidator, m.handleRegisterValidator).Methods(http.MethodPost)
	r.HandleFunc(pathGetHeader, m.handleGetHeader).Methods(http.MethodGet)
	r.HandleFunc(pathGetPayload, m.handleGetPayload).Methods(http.MethodPost)

	return m.newTestMiddleware(r)
}

// GetRequestCount returns the number of Request made to a specific URL
func (m *mockRelay) GetRequestCount(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requestCount[path]
}

// By default, handleRoot returns the relay's status
func (m *mockRelay) handleRoot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{}`)
}

// By default, handleStatus returns the relay's status as http.StatusOK
func (m *mockRelay) handleStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{}`)
}

// By default, handleRegisterValidator returns a default builderApiV1.SignedValidatorRegistration
func (m *mockRelay) handleRegisterValidator(w http.ResponseWriter, req *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handlerOverrideRegisterValidator != nil {
		m.handlerOverrideRegisterValidator(w, req)
		return
	}
	m.defaultHandleRegisterValidator(w, req)
}

// defaultHandleRegisterValidator returns the default handler for handleRegisterValidator
func (m *mockRelay) defaultHandleRegisterValidator(w http.ResponseWriter, req *http.Request) {
	payload := []builderApiV1.SignedValidatorRegistration{}
	if err := DecodeJSON(req.Body, &payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
}

// MakeGetHeaderResponse is used to create the default or can be used to create a custom response to the getHeader
// method
func (m *mockRelay) MakeGetHeaderResponse(value uint64, blockHash, parentHash, publicKey string, version spec.DataVersion) *builderSpec.VersionedSignedBuilderBid {
	switch version {
	case spec.DataVersionCapella:
		// Fill the payload with custom values.
		message := &builderApiCapella.BuilderBid{
			Header: &capella.ExecutionPayloadHeader{
				BlockHash:       _HexToHash(blockHash),
				ParentHash:      _HexToHash(parentHash),
				WithdrawalsRoot: phase0.Root{},
			},
			Value:  uint256.NewInt(value),
			Pubkey: _HexToPubkey(publicKey),
		}

		// Sign the message.
		signature, err := ssz.SignMessage(message, ssz.DomainBuilder, m.secretKey)
		require.NoError(m.t, err)

		return &builderSpec.VersionedSignedBuilderBid{
			Version: spec.DataVersionCapella,
			Capella: &builderApiCapella.SignedBuilderBid{
				Message:   message,
				Signature: signature,
			},
		}
	case spec.DataVersionDeneb:
		message := &builderApiDeneb.BuilderBid{
			Header: &deneb.ExecutionPayloadHeader{
				BlockHash:       _HexToHash(blockHash),
				ParentHash:      _HexToHash(parentHash),
				WithdrawalsRoot: phase0.Root{},
				BaseFeePerGas:   uint256.NewInt(0),
			},
			BlobKZGCommitments: make([]deneb.KZGCommitment, 0),
			Value:              uint256.NewInt(value),
			Pubkey:             _HexToPubkey(publicKey),
		}

		// Sign the message.
		signature, err := ssz.SignMessage(message, ssz.DomainBuilder, m.secretKey)
		require.NoError(m.t, err)

		return &builderSpec.VersionedSignedBuilderBid{
			Version: spec.DataVersionDeneb,
			Deneb: &builderApiDeneb.SignedBuilderBid{
				Message:   message,
				Signature: signature,
			},
		}
	case spec.DataVersionUnknown, spec.DataVersionPhase0, spec.DataVersionAltair, spec.DataVersionBellatrix:
		return nil
	}
	return nil
}

func generateRandomHash() (common.Hash, error) {
	// Create a 32-byte array (since common.Hash is [32]byte)
	var hashBytes [32]byte

	// Fill the array with random bytes
	_, err := rand.Read(hashBytes[:])
	if err != nil {
		return common.Hash{}, err
	}

	// Convert the random bytes to a common.Hash and return it
	return common.BytesToHash(hashBytes[:]), nil
}

func MakeRandomExecutionPayload(numTx int) (*ExecutionPayload, error) {
	txs := make([]byte, numTx)
	for i := 0; i < numTx; i++ {
		randHash, err := generateRandomHash()
		if err != nil {
			return nil, err
		}

		txs = append(txs, randHash.Bytes()...)
	}

	return &ExecutionPayload{
		Transactions: txs,
	}, nil
}

func MakeRandomAnchorHeader(valueFloor int) (*AnchorHeader, error) {
	headerHash, err := generateRandomHash()
	if err != nil {
		return nil, err
	}

	blockHashStr, err := generateRandomHash()
	blockHash := blockHashStr.String()
	if err != nil {
		return nil, err
	}

	randValue := rand.Intn(10000) + valueFloor
	value := big.NewInt(int64(randValue))

	return &AnchorHeader{
		Header:    &headerHash,
		BlockHash: blockHash,
		Value:     value,
	}, nil
}

// MakeRandomAnchorGetHeaderResponse is used to create the default or can be used to create a custom response to the getHeader
func MakeRandomAnchorGetHeaderResponse(slot uint64) *AnchorGetHeaderResponse {
	tobHash, err := generateRandomHash()
	if err != nil {
		return nil
	}

	tobBlockHashStr, err := generateRandomHash()
	tobBlockHash := tobBlockHashStr.String()
	if err != nil {
		return nil
	}

	robHash1, err := generateRandomHash()
	if err != nil {
		return nil
	}

	robBlockHashStr1, err := generateRandomHash()
	robBlockHash1 := robBlockHashStr1.String()
	if err != nil {
		return nil
	}

	robHash2, err := generateRandomHash()
	if err != nil {
		return nil
	}

	robBlockHashStr2, err := generateRandomHash()
	robBlockHash2 := robBlockHashStr2.String()
	if err != nil {
		return nil
	}

	tobAnchorHeader := AnchorHeader{
		Header:    &tobHash,
		BlockHash: tobBlockHash,
		Value:     TestMockExpectedToBValue,
	}

	robAnchorHeader1 := AnchorHeader{
		Header:    &robHash1,
		BlockHash: robBlockHash1,
		Value:     TestMockExpectedRoBValue1,
	}

	robAnchorHeader2 := AnchorHeader{
		Header:    &robHash2,
		BlockHash: robBlockHash2,
		Value:     TestMockExpectedRoBValue2,
	}

	robHashes := make(map[string]*AnchorHeader, 0)
	robHashes[TestChainID1] = &robAnchorHeader1
	robHashes[TestChainID2] = &robAnchorHeader2

	execPayloads := ExecHeadersInfo{
		ToBHash:   &tobAnchorHeader,
		RoBHashes: robHashes,
	}

	anchorBlockInfo := AnchorBlockInfo{
		Slot: slot,
		// nodeID of chunk producing validator.
		Producer:       ids.NodeID{1},
		ProposerPubkey: *mockRelayPublicKey,
	}

	resp := AnchorGetHeaderResponse{
		ExecHeaders: execPayloads,
		BlockInfo:   anchorBlockInfo,
	}

	return &resp
}

// MakeAnchorGetHeaderResponse is to create a response msg with values set.
// Note a response may or may not have a ToB response.
func (m *mockRelay) MakeAnchorGetHeaderResponse(
	slot uint64,
	headersHash *common.Hash,
	tobHeader *AnchorHeader,
	robHeaders *map[string]*AnchorHeader,
) *AnchorGetHeaderResponse {
	resp := NewAnchorGetHeaderResponse()
	resp.BlockInfo.Slot = slot
	resp.BlockInfo.ProposerPubkey = *mockRelayPublicKey

	// if headersHash != nil {
	// 	resp.HeadersHash = *headersHash
	// }

	if tobHeader != nil {
		resp.ExecHeaders.ToBHash = tobHeader
	}

	if robHeaders != nil {
		resp.ExecHeaders.RoBHashes = *robHeaders
	}

	return resp
}

// handleGetHeader handles incoming requests to server.pathGetHeader
func (m *mockRelay) handleGetHeader(w http.ResponseWriter, req *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Try to override default behavior is custom handler is specified.
	if m.handlerOverrideGetHeader != nil {
		m.handlerOverrideGetHeader(w, req)
		return
	}
	m.defaultHandleGetHeader(w)
}

// defaultHandleGetHeader returns the default handler for handleGetHeader
func (m *mockRelay) defaultHandleGetHeader(w http.ResponseWriter) {
	// By default, everything will be ok.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	// Build the default response.
	var response *AnchorGetHeaderResponse
	if m.GetHeaderResponse != nil {
		response = m.GetHeaderResponse
	} else {
		response = MakeRandomAnchorGetHeaderResponse(1)
	}

	err := SignAnchorGetHeaderResponse(response, m.secretKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// MakeAnchorGetPayloadResponse is used to create the default or can be used to create a custom response to the getPayload
func MakeAnchorGetPayloadResponse(
	slot uint64,
) *AnchorGetPayloadResponse {
	resp := AnchorGetPayloadResponse{
		Slot: slot,
	}

	return &resp
}

func MakeRandomAnchorGetPayloadResponse(
	slot uint64,
	numTxs int,
	addToBPayload bool,
	robChainIDs []string,
) (*AnchorGetPayloadResponse, error) {
	resp := AnchorGetPayloadResponse{
		Slot: slot,
	}

	var tobPayload *ExecutionPayload
	var err error
	if addToBPayload {
		tobPayload, err = MakeRandomExecutionPayload(numTxs)
		if err != nil {
			return nil, err
		}
	}

	robPayloads := make(map[string]ExecutionPayload)

	for _, robChainID := range robChainIDs {
		robPayload, err := MakeRandomExecutionPayload(numTxs)
		if err != nil {
			return nil, err
		}
		robPayloads[robChainID] = *robPayload
	}

	resp.ExecPayloads.ToBPayload = tobPayload
	resp.ExecPayloads.RoBPayloads = robPayloads

	return &resp, nil
}

// handleGetPayload handles incoming requests to server.pathGetPayload
func (m *mockRelay) handleGetPayload(w http.ResponseWriter, req *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Try to override default behavior is custom handler is specified.
	if m.handlerOverrideGetPayload != nil {
		m.handlerOverrideGetPayload(w, req)
		return
	}
	m.defaultHandleGetPayload(w)
}

// defaultHandleGetPayload returns the default handler for handleGetPayload
func (m *mockRelay) defaultHandleGetPayload(w http.ResponseWriter) {
	// By default, everything will be ok.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	// Build the default response.
	robChainIDs := make([]string, 0)
	robChainIDs = append(robChainIDs, TestChainID1)
	robChainIDs = append(robChainIDs, TestChainID2)

	response, err := MakeRandomAnchorGetPayloadResponse(1, 1, true, robChainIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if m.GetPayloadResponse != nil {
		response = m.GetPayloadResponse
	}

	err = SignAnchorGetPayloadResponse(response, m.secretKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (m *mockRelay) overrideHandleRegisterValidator(method func(w http.ResponseWriter, req *http.Request)) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.handlerOverrideRegisterValidator = method
}
