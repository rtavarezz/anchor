package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/flashbots/go-boost-utils/bls"

	builderApiV1 "github.com/attestantio/go-builder-client/api/v1"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/flashbots/go-boost-utils/types"
	"github.com/stretchr/testify/require"
)

const (
	LowerThanMinBidFloor  = 0
	LargerThanMinBidFloor = 20000
	TestRelayTimeout      = 10
)

type testBackend struct {
	boost  *AnchorService
	relays []*mockRelay
}

// newTestBackend creates a new backend, initializes mock relays, registers them and return the instance
func newTestBackend(t *testing.T, numRelays int, relayTimeout time.Duration) *testBackend {
	t.Helper()
	backend := testBackend{
		relays: make([]*mockRelay, numRelays),
	}

	relayEntries := make([]RelayEntry, numRelays)
	for i := 0; i < numRelays; i++ {
		// Create a mock relay
		backend.relays[i] = newMockRelay(t)
		relayEntries[i] = backend.relays[i].RelayEntry
		relayEntries[i].PublicKey = mockRelayPublicKey.Bytes()
	}

	opts := AnchorServiceOpts{
		Log:                      testLog,
		ListenAddr:               "localhost:12345",
		Relays:                   relayEntries,
		GenesisForkVersionHex:    "0x00000000",
		RelayCheck:               true,
		RelayMinBid:              types.IntToU256(12345),
		RequestTimeoutGetHeader:  relayTimeout,
		RequestTimeoutGetPayload: relayTimeout,
		RequestTimeoutRegVal:     relayTimeout,
		RequestMaxRetries:        5,
		MockMode:                 true,
	}
	service, err := NewAnchorService(opts)
	require.NoError(t, err)

	backend.boost = service
	return &backend
}

func (be *testBackend) request(t *testing.T, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	var err error

	if payload == nil {
		req, err = http.NewRequest(method, path, bytes.NewReader(nil))
	} else {
		payloadBytes, err2 := json.Marshal(payload)
		require.NoError(t, err2)
		req, err = http.NewRequest(method, path, bytes.NewReader(payloadBytes))
	}

	require.NoError(t, err)
	rr := httptest.NewRecorder()
	be.boost.getRouter().ServeHTTP(rr, req)
	return rr
}

func TestNewBoostServiceErrors(t *testing.T) {
	t.Run("errors when no relays", func(t *testing.T) {
		_, err := NewAnchorService(AnchorServiceOpts{
			Log:                      testLog,
			ListenAddr:               ":123",
			Relays:                   []RelayEntry{},
			RelayMonitors:            []*url.URL{},
			GenesisForkVersionHex:    "0x00000000",
			GenesisTime:              0,
			RelayCheck:               true,
			RelayMinBid:              types.IntToU256(0),
			RequestTimeoutGetHeader:  time.Second,
			RequestTimeoutGetPayload: time.Second,
			RequestTimeoutRegVal:     time.Second,
			RequestMaxRetries:        1,
		})
		require.Error(t, err)
	})
}

func TestWebserver(t *testing.T) {
	t.Run("errors when webserver is already existing", func(t *testing.T) {
		backend := newTestBackend(t, 1, time.Second*TestRelayTimeout)
		backend.boost.srv = &http.Server{}
		err := backend.boost.StartHTTPServer()
		require.Error(t, err)
	})

	t.Run("webserver error on invalid listenAddr", func(t *testing.T) {
		backend := newTestBackend(t, 1, time.Second*TestRelayTimeout)
		backend.boost.listenAddr = "localhost:876543"
		err := backend.boost.StartHTTPServer()
		require.Error(t, err)
	})

	t.Run("webserver starts normally", func(t *testing.T) {
		backend := newTestBackend(t, 1, time.Second*TestRelayTimeout)
		go func() {
			err := backend.boost.StartHTTPServer()
			require.NoError(t, err)
		}()
		time.Sleep(time.Millisecond * 100)
		err := backend.boost.srv.Close()
		require.NoError(t, err)
	})
}

func TestWebserverRootHandler(t *testing.T) {
	backend := newTestBackend(t, 1, time.Second*TestRelayTimeout)

	// Check root handler
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	backend.boost.getRouter().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "{}\n", rr.Body.String())
}

func TestWebserverMaxHeaderSize(t *testing.T) {
	backend := newTestBackend(t, 1, time.Second*TestRelayTimeout)
	addr := "localhost:1234"
	backend.boost.listenAddr = addr
	go func() {
		err := backend.boost.StartHTTPServer()
		require.NoError(t, err)
	}()
	time.Sleep(time.Millisecond * 100)
	path := "http://" + addr + "?" + strings.Repeat("abc", 4000) // path with characters of size over 4kb
	code, err := SendHTTPRequest(context.Background(), *http.DefaultClient, http.MethodGet, path, "test", nil, nil, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusRequestHeaderFieldsTooLarge, code)
	err = backend.boost.srv.Close()
	require.NoError(t, err)
}

func TestStatus(t *testing.T) {
	t.Run("At least one relay is available", func(t *testing.T) {
		backend := newTestBackend(t, 1, time.Second*TestRelayTimeout)
		time.Sleep(time.Millisecond * 20)
		path := "/eth/v1/builder/status"
		rr := backend.request(t, http.MethodGet, path, nil)

		require.Equal(t, http.StatusOK, rr.Code)
		require.Greater(t, len(rr.Header().Get("X-MEVBoost-Version")), 0) //nolint:testifylint
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
	})

	t.Run("No relays available", func(t *testing.T) {
		backend := newTestBackend(t, 1, time.Second*TestRelayTimeout)
		backend.relays[0].Server.Close() // makes the relay unavailable

		path := "/eth/v1/builder/status"
		rr := backend.request(t, http.MethodGet, path, nil)

		require.Equal(t, http.StatusServiceUnavailable, rr.Code)
		require.Greater(t, len(rr.Header().Get("X-MEVBoost-Version")), 0) //nolint:testifylint
		require.Equal(t, 0, backend.relays[0].GetRequestCount(path))
	})
}

func TestRegisterValidator(t *testing.T) {
	path := "/eth/v1/builder/validators"
	reg := builderApiV1.SignedValidatorRegistration{
		Message: &builderApiV1.ValidatorRegistration{
			FeeRecipient: _HexToAddress("0xdb65fEd33dc262Fe09D9a2Ba8F80b329BA25f941"),
			Timestamp:    time.Unix(1234356, 0),
			Pubkey: _HexToPubkey(
				"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249"),
		},
		Signature: _HexToSignature(
			"0x81510b571e22f89d1697545aac01c9ad0c1e7a3e778b3078bef524efae14990e58a6e960a152abd49de2e18d7fd3081c15d5c25867ccfad3d47beef6b39ac24b6b9fbf2cfa91c88f67aff750438a6841ec9e4a06a94ae41410c4f97b75ab284c"),
	}
	payload := []builderApiV1.SignedValidatorRegistration{reg}

	t.Run("Normal function", func(t *testing.T) {
		backend := newTestBackend(t, 1, time.Second*TestRelayTimeout)
		rr := backend.request(t, http.MethodPost, path, payload)
		require.Equal(t, http.StatusOK, rr.Code)
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
	})

	t.Run("Relay error response", func(t *testing.T) {
		backend := newTestBackend(t, 2, time.Second*TestRelayTimeout)

		backend.relays[0].ResponseDelay = 5 * time.Millisecond
		backend.relays[1].ResponseDelay = 5 * time.Millisecond

		rr := backend.request(t, http.MethodPost, path, payload)
		require.Equal(t, http.StatusOK, rr.Code)
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 1, backend.relays[1].GetRequestCount(path))

		// Now make one relay return an error
		backend.relays[0].overrideHandleRegisterValidator(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
		})
		rr = backend.request(t, http.MethodPost, path, payload)
		require.Equal(t, http.StatusOK, rr.Code)
		require.Equal(t, 2, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 2, backend.relays[1].GetRequestCount(path))

		// Now make both relays return an error - which should cause the request to fail
		backend.relays[1].overrideHandleRegisterValidator(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
		})
		rr = backend.request(t, http.MethodPost, path, payload)
		require.Equal(t, `{"code":502,"message":"no successful relay response"}`+"\n", rr.Body.String())
		require.Equal(t, http.StatusBadGateway, rr.Code)
		require.Equal(t, 3, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 3, backend.relays[1].GetRequestCount(path))
	})

	t.Run("mev-boost relay timeout works with slow relay", func(t *testing.T) {
		backend := newTestBackend(t, 1, 150*time.Millisecond) // 10ms max
		rr := backend.request(t, http.MethodPost, path, payload)
		require.Equal(t, http.StatusOK, rr.Code)

		// Now make the relay return slowly, mev-boost should return an error
		backend.relays[0].ResponseDelay = 180 * time.Millisecond
		rr = backend.request(t, http.MethodPost, path, payload)
		require.Equal(t, `{"code":502,"message":"no successful relay response"}`+"\n", rr.Body.String())
		require.Equal(t, http.StatusBadGateway, rr.Code)
		require.Equal(t, 2, backend.relays[0].GetRequestCount(path))
	})
}

func getHeaderPath(slot uint64, parentHash ids.ID, pubkey phase0.BLSPubKey) string {
	return fmt.Sprintf("/eth/v1/builder/header/%d/%s/%s", slot, parentHash.String(), pubkey.String())
}

func TestVerifyHeaderSignatures(t *testing.T) {
	secretKey, err := bls.GenerateRandomSecretKey()
	require.NoError(t, err)

	// Sign the exec payloads
	headerMsg := MakeRandomAnchorGetHeaderResponse(1)
	signature, err := GetExecHeaderSignature(&headerMsg.ExecHeaders, secretKey)
	require.NoError(t, err)
	headerMsg.SetExecPayloadsSig(signature)

	// Verify the signature matches
	pubKey, err := bls.PublicKeyFromSecretKey(secretKey)
	require.NoError(t, err)

	headerIsVerified, err := VerifyHeaderSignature(headerMsg, *pubKey)
	require.NoError(t, err)
	require.True(t, headerIsVerified)
}

func TestVerifyPayloadSignatures(t *testing.T) {
	secretKey, err := bls.GenerateRandomSecretKey()
	require.NoError(t, err)

	// Sign the exec payloads
	robChainIDS := make([]string, 0)
	robChainIDS = append(robChainIDS, "chain_1")
	robChainIDS = append(robChainIDS, "chain_2")

	payloadRespMsg, err := MakeRandomAnchorGetPayloadResponse(uint64(1), 2, true, robChainIDS)
	require.NoError(t, err)

	signature, err := GetExecPayloadSignature(&payloadRespMsg.ExecPayloads, secretKey)
	require.NoError(t, err)
	payloadRespMsg.SetExecPayloadsSig(signature)

	// Verify the signature matches
	pubKey, err := bls.PublicKeyFromSecretKey(secretKey)
	require.NoError(t, err)

	headerIsVerified, err := VerifyPayloadSignature(payloadRespMsg, *pubKey)
	require.NoError(t, err)
	require.True(t, headerIsVerified)
}

func TestGetHeader(t *testing.T) {
	hash := ids.Empty
	fmt.Printf("hash(%d): %s\n", len(hash.String()), hash.String())
	pubkey := _HexToPubkey(
		"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249")
	path := getHeaderPath(1, hash, pubkey)
	require.Equal(t, "/eth/v1/builder/header/1/11111111111111111111111111111111LpoYY/0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249", path)

	// TODO: to be removed
	t.Run("convert bls key", func(t *testing.T) {
		skHex := "0x3d8f5270b77bd2ab536a5a6155f52109f95d5f063269d89984d0b194e487bcb4"
		skBytes, err := hex.DecodeString(strings.TrimLeft(skHex, "0x"))
		require.NoError(t, err)
		sk, err := bls.SecretKeyFromBytes(skBytes)
		require.NoError(t, err)
		pk, err := bls.PublicKeyFromSecretKey(sk)
		require.NoError(t, err)
		pkBytes := pk.Bytes()
		pkStr := hexutil.Encode(pkBytes[:])
		fmt.Printf("pkStr: %s\n", pkStr)
	})

	t.Run("convert bls pubkey", func(t *testing.T) {
		pkHex := "b20ab07ea5cf8b77ab50f2071a6f1d2aab693c8bd89761430cd29de9fa0dbae83a32c7697d59ff1b06995b1596c16fa6"
		pkBytes, err := hex.DecodeString(pkHex)
		require.NoError(t, err)
		_, err = bls.PublicKeyFromBytes(pkBytes)
		require.NoError(t, err)
	})

	t.Run("Okay response from relay with both tob and rob", func(t *testing.T) {
		backend := newTestBackend(t, 1, time.Second*TestRelayTimeout)
		rr := backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
	})

	t.Run("Okay response from relay with both tob and rob", func(t *testing.T) {
		backend := newTestBackend(t, 1, time.Second*TestRelayTimeout)
		rr := backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
	})

	t.Run("Empty payload returns no status content", func(t *testing.T) {
		backend := newTestBackend(t, 2, time.Second*TestRelayTimeout)
		resp := backend.relays[0].MakeAnchorGetHeaderResponse(1, nil, nil, nil)

		// nil tob and rob will return no content.
		// only one baton is processed
		backend.relays[0].GetHeaderResponse = resp
		rr := backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
		require.Equal(t, http.StatusNoContent, rr.Code)

		backend.relays[1].GetHeaderResponse = resp
		rr = backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, 2, backend.relays[0].GetRequestCount(path))
		require.Equal(t, http.StatusNoContent, rr.Code)
	})

	t.Run("Good response from relays, tob and no robs", func(t *testing.T) {
		backend := newTestBackend(t, 2, time.Second*TestRelayTimeout)

		tobHeader, err := MakeRandomAnchorHeader(LargerThanMinBidFloor) // value must be over min req to be valid
		require.NoError(t, err)

		resp := backend.relays[0].MakeAnchorGetHeaderResponse(1, nil, tobHeader, nil)

		// nil tob and rob will return no content.
		// only one baton is processed
		backend.relays[0].GetHeaderResponse = resp

		rr := backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 0, backend.relays[1].GetRequestCount(path))
		require.Equal(t, http.StatusOK, rr.Code)

		backend.relays[1].GetHeaderResponse = resp
		rr = backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, 2, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 0, backend.relays[1].GetRequestCount(path))
		require.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("Only tob but below floor", func(t *testing.T) {
		backend := newTestBackend(t, 2, time.Second*TestRelayTimeout)

		tobHeader, err := MakeRandomAnchorHeader(LowerThanMinBidFloor) // value will be below floor
		require.NoError(t, err)

		resp := backend.relays[0].MakeAnchorGetHeaderResponse(1, nil, tobHeader, nil)

		// nil tob and rob will return no content.
		// only one baton is processed
		backend.relays[0].GetHeaderResponse = resp
		rr := backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 0, backend.relays[1].GetRequestCount(path))
		require.Equal(t, http.StatusBadRequest, rr.Code)

		backend.relays[1].GetHeaderResponse = resp
		rr = backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, 2, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 0, backend.relays[1].GetRequestCount(path))
		require.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("one valid rob", func(t *testing.T) {
		backend := newTestBackend(t, 2, time.Second*TestRelayTimeout)

		robHeader, err := MakeRandomAnchorHeader(LargerThanMinBidFloor) // value will be below floor
		require.NoError(t, err)

		robHeaders := make(map[string]*AnchorHeader, 0)
		robHeaders[TestChainID1] = robHeader

		resp := backend.relays[0].MakeAnchorGetHeaderResponse(1, nil, nil, &robHeaders)

		// nil tob and rob will return no content.
		// only one baton is processed
		backend.relays[0].GetHeaderResponse = resp
		rr := backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 0, backend.relays[1].GetRequestCount(path))
		require.Equal(t, http.StatusOK, rr.Code)

		backend.relays[1].GetHeaderResponse = resp
		rr = backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, 2, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 0, backend.relays[1].GetRequestCount(path))
		require.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("one valid rob, one invalid tob, good overall", func(t *testing.T) {
		backend := newTestBackend(t, 2, time.Second*TestRelayTimeout)

		tobHeader, err := MakeRandomAnchorHeader(0) // value will be below floor
		require.NoError(t, err)

		robHeader, err := MakeRandomAnchorHeader(LargerThanMinBidFloor) // value will be below floor
		require.NoError(t, err)

		robHeaders := make(map[string]*AnchorHeader, 0)
		robHeaders[TestChainID1] = robHeader

		resp := backend.relays[0].MakeAnchorGetHeaderResponse(1, nil, tobHeader, &robHeaders)

		// nil tob and rob will return no content.
		// only one baton is processed
		backend.relays[0].GetHeaderResponse = resp
		rr := backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 0, backend.relays[1].GetRequestCount(path))
		require.Equal(t, http.StatusOK, rr.Code)

		backend.relays[1].GetHeaderResponse = resp
		rr = backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, 2, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 0, backend.relays[1].GetRequestCount(path))
		require.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("one invalid rob, one valid rob, one invalid tob, good overall", func(t *testing.T) {
		backend := newTestBackend(t, 2, time.Second*TestRelayTimeout)

		tobHeader, err := MakeRandomAnchorHeader(LowerThanMinBidFloor) // value will be below floor
		require.NoError(t, err)

		robHeader, err := MakeRandomAnchorHeader(LowerThanMinBidFloor) // value will be below floor
		require.NoError(t, err)

		robHeader2, err := MakeRandomAnchorHeader(LargerThanMinBidFloor) // value will be below floor
		require.NoError(t, err)

		robHeaders := make(map[string]*AnchorHeader)
		robHeaders[TestChainID1] = robHeader
		robHeaders[TestChainID2] = robHeader2

		resp := backend.relays[0].MakeAnchorGetHeaderResponse(1, nil, tobHeader, &robHeaders)

		// nil tob and rob will return no content.
		// only one baton is processed
		backend.relays[0].GetHeaderResponse = resp
		rr := backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 0, backend.relays[1].GetRequestCount(path))
		require.Equal(t, http.StatusOK, rr.Code)

		backend.relays[1].GetHeaderResponse = resp
		rr = backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, 2, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 0, backend.relays[1].GetRequestCount(path))
		require.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("Invalid relay public key in msg", func(t *testing.T) {
		backend := newTestBackend(t, 2, time.Second*TestRelayTimeout)

		tobHeader, err := MakeRandomAnchorHeader(LargerThanMinBidFloor) // value must be over min req to be valid
		require.NoError(t, err)

		resp := backend.relays[0].MakeAnchorGetHeaderResponse(1, nil, tobHeader, nil)

		// Simulate a different public key than the relays
		pk := bls.PublicKey{}
		pkBytes := pk.Bytes()
		resp.BlockInfo.ProposerPubkey = pkBytes[:]

		backend.relays[0].GetHeaderResponse = resp
		rr := backend.request(t, http.MethodGet, path, nil)
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
		require.Equal(t, http.StatusNoContent, rr.Code)
	})
	/*

		t.Run("Invalid relay signature", func(t *testing.T) {
			backend := newTestBackend(t, 1, time.Second * TestRelayTimeout)

			backend.relays[0].GetHeaderResponse = backend.relays[0].MakeGetHeaderResponse(
				12345,
				"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
				"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
				"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249",
				spec.DataVersionCapella,
			)

			// Scramble the signature
			backend.relays[0].GetHeaderResponse.Capella.Signature = phase0.BLSSignature{}

			rr := backend.request(t, http.MethodGet, path, nil)
			require.Equal(t, 1, backend.relays[0].GetRequestCount(path))

			// Request should have no content
			require.Equal(t, http.StatusNoContent, rr.Code)
		})

		t.Run("Invalid slot number", func(t *testing.T) {
			// Number larger than uint64 creates parsing error
			slot := fmt.Sprintf("%d0", uint64(math.MaxUint64))
			invalidSlotPath := fmt.Sprintf("/eth/v1/builder/header/%s/%s/%s", slot, hash.String(), pubkey.String())

			backend := newTestBackend(t, 1, time.Second * TestRelayTimeout)
			rr := backend.request(t, http.MethodGet, invalidSlotPath, nil)
			require.Equal(t, `{"code":400,"message":"invalid slot"}`+"\n", rr.Body.String())
			require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			require.Equal(t, 0, backend.relays[0].GetRequestCount(path))
		})

		t.Run("Invalid pubkey length", func(t *testing.T) {
			invalidPubkeyPath := fmt.Sprintf("/eth/v1/builder/header/%d/%s/%s", 1, hash.String(), "0x1")

			backend := newTestBackend(t, 1, time.Second * TestRelayTimeout)
			rr := backend.request(t, http.MethodGet, invalidPubkeyPath, nil)
			require.Equal(t, `{"code":400,"message":"invalid pubkey"}`+"\n", rr.Body.String())
			require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			require.Equal(t, 0, backend.relays[0].GetRequestCount(path))
		})

		t.Run("Invalid hash length", func(t *testing.T) {
			invalidSlotPath := fmt.Sprintf("/eth/v1/builder/header/%d/%s/%s", 1, "0x1", pubkey.String())

			backend := newTestBackend(t, 1, time.Second * TestRelayTimeout)
			rr := backend.request(t, http.MethodGet, invalidSlotPath, nil)
			require.Equal(t, `{"code":400,"message":"invalid hash"}`+"\n", rr.Body.String())
			require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			require.Equal(t, 0, backend.relays[0].GetRequestCount(path))
		})

		t.Run("Invalid parent hash", func(t *testing.T) {
			backend := newTestBackend(t, 1, time.Second * TestRelayTimeout)

			invalidParentHashPath := getHeaderPath(1, phase0.Hash32{}, pubkey)
			rr := backend.request(t, http.MethodGet, invalidParentHashPath, nil)
			require.Equal(t, http.StatusNoContent, rr.Code)
			require.Equal(t, 0, backend.relays[0].GetRequestCount(path))
		})
	*/
}

// TODO: FIX ME
/*
func TestGetHeaderBids(t *testing.T) {
	hash := _HexToHash("0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7")
	pubkey := _HexToPubkey(
		"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249")
	path := getHeaderPath(2, hash, pubkey)
	require.Equal(t, "/eth/v1/builder/header/2/0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7/0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249", path)

	t.Run("Use header with highest value", func(t *testing.T) {
		// Create backend and register 3 relays.
		backend := newTestBackend(t, 3, time.Second)

		// First relay will return signed response with value 12345.
		backend.relays[0].GetHeaderResponse = backend.relays[0].MakeGetHeaderResponse(
			12345,
			"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249",
			spec.DataVersionCapella,
		)

		// First relay will return signed response with value 12347.
		backend.relays[1].GetHeaderResponse = backend.relays[1].MakeGetHeaderResponse(
			12347,
			"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249",
			spec.DataVersionCapella,
		)

		// First relay will return signed response with value 12346.
		backend.relays[2].GetHeaderResponse = backend.relays[2].MakeGetHeaderResponse(
			12346,
			"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249",
			spec.DataVersionCapella,
		)

		// Run the request.
		rr := backend.request(t, http.MethodGet, path, nil)

		// Each relay must have received the request.
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 1, backend.relays[1].GetRequestCount(path))
		require.Equal(t, 1, backend.relays[2].GetRequestCount(path))

		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		// Highest value should be 12347, i.e. second relay.
		resp := new(builderSpec.VersionedSignedBuilderBid)
		err := json.Unmarshal(rr.Body.Bytes(), resp)
		require.NoError(t, err)
		value, err := resp.Value()
		require.NoError(t, err)
		require.Equal(t, uint256.NewInt(12347), value)
	})

	t.Run("Use header with lowest blockhash if same value", func(t *testing.T) {
		// Create backend and register 3 relays.
		backend := newTestBackend(t, 3, time.Second * TestRelayTimeout)

		backend.relays[0].GetHeaderResponse = backend.relays[0].MakeGetHeaderResponse(
			12345,
			"0xa38385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249",
			spec.DataVersionCapella,
		)

		backend.relays[1].GetHeaderResponse = backend.relays[1].MakeGetHeaderResponse(
			12345,
			"0xa18385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249",
			spec.DataVersionCapella,
		)

		backend.relays[2].GetHeaderResponse = backend.relays[2].MakeGetHeaderResponse(
			12345,
			"0xa28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249",
			spec.DataVersionCapella,
		)

		// Run the request.
		rr := backend.request(t, http.MethodGet, path, nil)

		// Each relay must have received the request.
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
		require.Equal(t, 1, backend.relays[1].GetRequestCount(path))
		require.Equal(t, 1, backend.relays[2].GetRequestCount(path))

		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		// Highest value should be 12347, i.e. second relay.
		resp := new(builderSpec.VersionedSignedBuilderBid)

		err := json.Unmarshal(rr.Body.Bytes(), resp)
		require.NoError(t, err)
		value, err := resp.Value()
		require.NoError(t, err)
		require.Equal(t, uint256.NewInt(12345), value)
		blockHash, err := resp.BlockHash()
		require.NoError(t, err)
		require.Equal(t, "0xa18385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7", blockHash.String())
	})

	t.Run("Respect minimum bid cutoff", func(t *testing.T) {
		// Create backend and register relay.
		backend := newTestBackend(t, 1, time.Second)

		// Relay will return signed response with value 12344.
		backend.relays[0].GetHeaderResponse = backend.relays[0].MakeGetHeaderResponse(
			12344,
			"0xa28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249",
			spec.DataVersionCapella,
		)

		// Run the request.
		rr := backend.request(t, http.MethodGet, path, nil)

		// Each relay must have received the request.
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))

		// Request should have no content (min bid is 12345)
		require.Equal(t, http.StatusNoContent, rr.Code)
	})

	t.Run("Allow bids which meet minimum bid cutoff", func(t *testing.T) {
		// Create backend and register relay.
		backend := newTestBackend(t, 1, time.Second)

		// First relay will return signed response with value 12345.
		backend.relays[0].GetHeaderResponse = backend.relays[0].MakeGetHeaderResponse(
			12345,
			"0xa28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7",
			"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249",
			spec.DataVersionCapella,
		)

		// Run the request.
		rr := backend.request(t, http.MethodGet, path, nil)

		// Each relay must have received the request.
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))

		// Value should be 12345 (min bid is 12345)
		resp := new(builderSpec.VersionedSignedBuilderBid)
		err := json.Unmarshal(rr.Body.Bytes(), resp)
		require.NoError(t, err)
		value, err := resp.Value()
		require.NoError(t, err)
		require.Equal(t, uint256.NewInt(12345), value)
	})
}
*/

func TestGetPayload(t *testing.T) {
	path := "/eth/v1/builder/blinded_blocks"

	parentHash, err := generateRandomHash()
	require.Nil(t, err)

	pubKeyBytes := mockRelayPublicKey.Bytes()
	payloadReq := AnchorGetPayloadRequest{
		Slot:           1,
		ProposerPubKey: pubKeyBytes[:],
		ParentHash:     parentHash.String(),
	}

	t.Run("Okay response from relay", func(t *testing.T) {
		backend := newTestBackend(t, 1, time.Second*30)
		rr := backend.request(t, http.MethodPost, path, payloadReq)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		require.Equal(t, 1, backend.relays[0].GetRequestCount(path))

		resp := new(AnchorGetPayloadResponse)
		err := json.Unmarshal(rr.Body.Bytes(), resp)
		require.NoError(t, err)
		require.Equal(t, true, resp.HasToBTxs())
		require.Equal(t, 2, resp.NumRoBChains())
	})

	/*
		t.Run("Empty response from relay", func(t *testing.T) {
			backend := newTestBackend(t, 1, time.Second*30)

			resp := MakeRandomAnchorGetHeaderResponse(1)
			rr := backend.request(t, http.MethodPost, path, payloadReq)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			require.Equal(t, 1, backend.relays[0].GetRequestCount(path))

			resp := new(AnchorGetPayloadResponse)
			err := json.Unmarshal(rr.Body.Bytes(), resp)
			require.NoError(t, err)
			require.Equal(t, DefaultTestPayloadNumToBTxs, resp.HasToBTxs())
			require.Equal(t, DefaultTestPayloadNumRoBTxs, resp.NumRoBChains())
			//require.Equal(t, payload.Message.Body.ExecutionPayloadHeader.BlockHash, resp.Capella.BlockHash)
		})
	*/

	/*
		t.Run("Bad response from relays", func(t *testing.T) {
			backend := newTestBackend(t, 2, time.Second)
			resp := &builderApi.VersionedSubmitBlindedBlockResponse{
				Version: spec.DataVersionCapella,
				Capella: &capella.ExecutionPayload{Withdrawals: []*capella.Withdrawal{}},
			}

			// 1/2 failing responses are okay
			backend.relays[0].GetPayloadResponse = resp
			rr := backend.request(t, http.MethodPost, path, payload)
			require.GreaterOrEqual(t, backend.relays[1].GetRequestCount(path)+backend.relays[0].GetRequestCount(path), 1)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

			// 2/2 failing responses are okay
			backend = newTestBackend(t, 2, time.Second)
			backend.relays[0].GetPayloadResponse = resp
			backend.relays[1].GetPayloadResponse = resp
			rr = backend.request(t, http.MethodPost, path, payload)
			require.Equal(t, 1, backend.relays[0].GetRequestCount(path))
			require.Equal(t, 1, backend.relays[1].GetRequestCount(path))
			require.Equal(t, `{"code":502,"message":"no successful relay response"}`+"\n", rr.Body.String())
			require.Equal(t, http.StatusBadGateway, rr.Code, rr.Body.String())
		})

		t.Run("Retries on error from relay", func(t *testing.T) {
			backend := newTestBackend(t, 1, 2*time.Second)

			count := 0
			backend.relays[0].handlerOverrideGetPayload = func(w http.ResponseWriter, r *http.Request) {
				if count > 0 {
					// success response on the second attempt
					backend.relays[0].defaultHandleGetPayload(w)
				} else {
					w.WriteHeader(http.StatusInternalServerError)
					_, err := w.Write([]byte(`{"code":500,"message":"internal server error"}`))
					require.NoError(t, err)
				}
				count++
			}
			rr := backend.request(t, http.MethodPost, path, payload)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		})

		t.Run("Error after max retries are reached", func(t *testing.T) {
			backend := newTestBackend(t, 1, 0)

			count := 0
			maxRetries := 5

			backend.relays[0].handlerOverrideGetPayload = func(w http.ResponseWriter, r *http.Request) {
				count++
				if count > maxRetries {
					// success response after max retry attempts
					backend.relays[0].defaultHandleGetPayload(w)
				} else {
					w.WriteHeader(http.StatusInternalServerError)
					_, err := w.Write([]byte(`{"code":500,"message":"internal server error"}`))
					require.NoError(t, err)
				}
			}
			rr := backend.request(t, http.MethodPost, path, payload)
			require.Equal(t, 5, backend.relays[0].GetRequestCount(path))
			require.Equal(t, `{"code":502,"message":"no successful relay response"}`+"\n", rr.Body.String())
			require.Equal(t, http.StatusBadGateway, rr.Code, rr.Body.String())
		})

	*/
}

func TestCheckRelays(t *testing.T) {
	t.Run("One relay is okay", func(t *testing.T) {
		backend := newTestBackend(t, 1, time.Second)
		numHealthyRelays := backend.boost.CheckRelays()
		require.Equal(t, 1, numHealthyRelays)
	})

	t.Run("One relay is down", func(t *testing.T) {
		backend := newTestBackend(t, 1, time.Second)
		backend.relays[0].Server.Close()

		numHealthyRelays := backend.boost.CheckRelays()
		require.Equal(t, 0, numHealthyRelays)
	})

	t.Run("One relays is up, one down", func(t *testing.T) {
		backend := newTestBackend(t, 2, time.Second)
		backend.relays[0].Server.Close()

		numHealthyRelays := backend.boost.CheckRelays()
		require.Equal(t, 1, numHealthyRelays)
	})

	t.Run("Should not follow redirects", func(t *testing.T) {
		backend := newTestBackend(t, 1, time.Second)
		redirectAddress := backend.relays[0].Server.URL
		backend.relays[0].Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, redirectAddress, http.StatusTemporaryRedirect)
		}))

		url, err := url.ParseRequestURI(backend.relays[0].Server.URL)
		require.NoError(t, err)
		backend.boost.relays[0].URL = url
		numHealthyRelays := backend.boost.CheckRelays()
		require.Equal(t, 0, numHealthyRelays)
	})
}

// TODO: Fix me
/*
func TestGetPayloadToAllRelays(t *testing.T) {
	// Load the signed blinded beacon block used for getPayload
	jsonFile, err := os.Open("../testdata/signed-blinded-beacon-block-capella.json")
	require.NoError(t, err)
	defer jsonFile.Close()
	signedBlindedBeaconBlock := new(eth2ApiV1Capella.SignedBlindedBeaconBlock)
	require.NoError(t, DecodeJSON(jsonFile, &signedBlindedBeaconBlock))

	// Create a test backend with 2 relays
	backend := newTestBackend(t, 2, time.Second)

	// call getHeader, highest bid is returned by relay 0
	getHeaderPath := "/eth/v1/builder/header/12345/0xcf8e0d4e9587369b2301d0790347320302cc0943d5a1884560367e8208d920f2/0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249"
	backend.relays[0].GetHeaderResponse = backend.relays[0].MakeGetHeaderResponse(
		12345,
		"0xcf8e0d4e9587369b2301d0790347320302cc0943d5a1884560367e8208d920f2",
		"0xcf8e0d4e9587369b2301d0790347320302cc0943d5a1884560367e8208d920f2",
		"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249",
		spec.DataVersionCapella,
	)
	rr := backend.request(t, http.MethodGet, getHeaderPath, nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Equal(t, 1, backend.relays[0].GetRequestCount(getHeaderPath))
	require.Equal(t, 1, backend.relays[1].GetRequestCount(getHeaderPath))

	// Prepare getPayload response
	backend.relays[0].GetPayloadResponse = &builderApi.VersionedSubmitBlindedBlockResponse{
		Version: spec.DataVersionCapella,
		Capella: blindedBlockToExecutionPayloadCapella(signedBlindedBeaconBlock),
	}

	// call getPayload, ensure it's called to all relays
	getPayloadPath := "/eth/v1/builder/blinded_blocks"
	rr = backend.request(t, http.MethodPost, getPayloadPath, signedBlindedBeaconBlock)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Equal(t, 1, backend.relays[0].GetRequestCount(getPayloadPath))
	require.Equal(t, 1, backend.relays[1].GetRequestCount(getPayloadPath))
}
*/

/*
func TestMockAnchor(t *testing.T) {
	hash := _HexToHash("0xe28385e7bd68df656cd0042b74b69c3104b5356ed1f20eb69f1f925df47a3ab7")
	pubkey := _HexToPubkey(
		"0x8a1d7b8dd64e0aafe7ea7b6c95065c9364cf99d38470c12ee807d55f7de1529ad29ce2c422e0b65e3d5a05c02caca249")
	numTobTxs := 3
	numRoBChains := 3
	numRoBChunkTxs := 4

	path := getHeaderPath2(1, hash, pubkey, numTobTxs, numRoBChains, numRoBChunkTxs)
	backend := newTestBackend(t, 1, time.Second)
	rr := backend.request(t, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var testResponse SEQHeaderResponse
	err := testResponse.FromJSON(rr.Body.Bytes())
	require.NoError(t, err)

	require.Equal(t, 1, int(testResponse.Slot))
	require.Equal(t, numRoBChains, len(testResponse.RoBHashes))

	payloadReq := NewSEQPayloadRequest(1)

	path2 := getPayloadPath2()
	rr2 := backend.request(t, http.MethodPost, path2, payloadReq)
	require.Equal(t, http.StatusOK, rr2.Code, rr2.Body.String())

	// Below doesn't work after recent seq mock change. Shouldn't matter as we will get rid of this flow anyway.
	//var testPayloadRes SEQPayloadResponse
	//err = testPayloadRes.FromJSON(rr2.Body.Bytes())
	//require.NoError(t, err)

	//require.Equal(t, numTobTxs, len(testPayloadRes.ToBPayload.Transactions))
	//require.Equal(t, numRoBChains, len(testPayloadRes.RoBPayloads))
	//for _, v := range testPayloadRes.RoBPayloads {
	//	require.Equal(t, numRoBChunkTxs, len(v.Transactions))
	//}
}
*/
