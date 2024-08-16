package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	seqconsts "github.com/AnomalyFi/nodekit-seq/consts"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"golang.org/x/exp/rand"

	"github.com/AnomalyFi/hypersdk/chain"
	"github.com/AnomalyFi/nodekit-seq/consts"
	"github.com/AnomalyFi/nodekit-seq/genesis"

	boostUtils "github.com/flashbots/go-boost-utils/utils"

	"github.com/AnomalyFi/anchor/config"
	builderApi "github.com/attestantio/go-builder-client/api"
	builderApiV1 "github.com/attestantio/go-builder-client/api/v1"
	builderSpec "github.com/attestantio/go-builder-client/spec"
	eth2ApiV1Bellatrix "github.com/attestantio/go-eth2-client/api/v1/bellatrix"
	eth2ApiV1Capella "github.com/attestantio/go-eth2-client/api/v1/capella"
	eth2ApiV1Deneb "github.com/attestantio/go-eth2-client/api/v1/deneb"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	core "github.com/ethereum/go-ethereum/core/types"
	"github.com/flashbots/go-boost-utils/ssz"
	flash "github.com/flashbots/go-boost-utils/types"
	"github.com/flashbots/go-utils/httplogger"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

var (
	errNoRelays                  = errors.New("no relays")
	errInvalidSlot               = errors.New("invalid slot")
	errInvalidHash               = errors.New("invalid hash")
	errInvalidPubkey             = errors.New("invalid pubkey")
	errNoSuccessfulRelayResponse = errors.New("no successful relay response")
	errServerAlreadyRunning      = errors.New("server already running")
)

var (
	nonce uint64
	value big.Int
	gasLimit uint64
	gasPrice big.Int
	data string
)

const (
	TestPrivateKeyValue = "77619a19a837f894fa5c90e58ee3e3d69e382936d323d987bbde923da92a5ac5"
	TestAddressValue    = "0x59131f2c045f70Be0dDA50D86b6ED2b18C5012cf"
  )

var (
	nilHash     = phase0.Hash32{}
	nilResponse = struct{}{}
)

type httpErrorResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// AuctionTranscript is the bid and blinded block received from the relay send to the relay monitor
type AuctionTranscript struct {
	Bid        *builderSpec.VersionedSignedBuilderBid       // TODO: proper json marshalling and unmarshalling
	Acceptance *eth2ApiV1Bellatrix.SignedBlindedBeaconBlock `json:"acceptance"`
}

type slotUID struct {
	slot uint64
	uid  uuid.UUID
}

// BoostServiceOpts provides all available options for use with NewBoostService
type BoostServiceOpts struct {
	Log                   *logrus.Entry
	ListenAddr            string
	Relays                []RelayEntry
	RelayMonitors         []*url.URL
	GenesisForkVersionHex string
	GenesisTime           uint64
	RelayCheck            bool
	RelayMinBid           flash.U256Str

	RequestTimeoutGetHeader  time.Duration
	RequestTimeoutGetPayload time.Duration
	RequestTimeoutRegVal     time.Duration
	RequestMaxRetries        int
}

// BoostService - the mev-boost service
type BoostService struct {
	listenAddr    string
	relays        []RelayEntry
	relayMonitors []*url.URL
	log           *logrus.Entry
	srv           *http.Server
	relayCheck    bool
	relayMinBid   flash.U256Str
	genesisTime   uint64

	builderSigningDomain   phase0.Domain
	httpClientGetHeader    http.Client
	httpClientGetPayload   http.Client
	httpClientOPGetPayload http.Client
	httpClientRegVal       http.Client
	requestMaxRetries      int

	bids     map[bidRespKey]bidResp   // keeping track of bids, to log the originating relay on withholding
	opBids   map[bidRespKey]opBidResp // keeping track of bids, to log the originating relay on withholding
	bidsLock sync.Mutex

	slotUID     *slotUID
	slotUIDLock sync.Mutex
	mockChunkToB []hexutil.Bytes
	mockChunkRoB map[string][]hexutil.Bytes
	mockNonce 	uint64

}

// go program calls this on boot up automatically
func init() {
	rand.Seed(uint64(time.Now().UnixNano()))
}

// NewBoostService created a new BoostService
func NewBoostService(opts BoostServiceOpts) (*BoostService, error) {
	if len(opts.Relays) == 0 {
		return nil, errNoRelays
	}

	builderSigningDomain, err := ComputeDomain(ssz.DomainTypeAppBuilder, opts.GenesisForkVersionHex, phase0.Root{}.String())
	if err != nil {
		return nil, err
	}

	return &BoostService{
		listenAddr:    opts.ListenAddr,
		relays:        opts.Relays,
		relayMonitors: opts.RelayMonitors,
		log:           opts.Log,
		relayCheck:    opts.RelayCheck,
		relayMinBid:   opts.RelayMinBid,
		genesisTime:   opts.GenesisTime,
		bids:          make(map[bidRespKey]bidResp),
		slotUID:       &slotUID{},

		builderSigningDomain: builderSigningDomain,
		httpClientGetHeader: http.Client{
			Timeout:       opts.RequestTimeoutGetHeader,
			CheckRedirect: httpClientDisallowRedirects,
		},
		httpClientGetPayload: http.Client{
			Timeout:       opts.RequestTimeoutGetPayload,
			CheckRedirect: httpClientDisallowRedirects,
		},
		httpClientOPGetPayload: http.Client{
			Timeout:       opts.RequestTimeoutGetPayload,
			CheckRedirect: httpClientDisallowRedirects,
		},
		httpClientRegVal: http.Client{
			Timeout:       opts.RequestTimeoutRegVal,
			CheckRedirect: httpClientDisallowRedirects,
		},
		requestMaxRetries: opts.RequestMaxRetries,
	}, nil
}

func (m *BoostService) respondError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp := httpErrorResp{code, message}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		m.log.WithField("response", resp).WithError(err).Error("Couldn't write error response")
		http.Error(w, "", http.StatusInternalServerError)
	}
}

// note
func (m *BoostService) respondOK(w http.ResponseWriter, response any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		m.log.WithField("response", response).WithError(err).Error("Couldn't write OK response")
		http.Error(w, "", http.StatusInternalServerError)
	}
}

func (m *BoostService) getRouter() http.Handler {
	r := mux.NewRouter()
	r.HandleFunc("/", m.handleRoot)

	r.HandleFunc(pathStatus, m.handleStatus).Methods(http.MethodGet)
	r.HandleFunc(pathRegisterValidator, m.handleRegisterValidator).Methods(http.MethodPost)
	r.HandleFunc(pathGetHeader, m.handleGetHeader).Methods(http.MethodGet)
	r.HandleFunc(pathGetPayload, m.handleGetPayload).Methods(http.MethodPost)
	r.HandleFunc(pathGetOPPayload, m.handleOPGetPayload).Methods(http.MethodGet)

	// These are mock handlers for the SEQ-Anchor interface. This will be stubbed in for now.
	r.HandleFunc(pathGetHeader2, m.handleGetHeader2).Methods(http.MethodGet)
	r.HandleFunc(pathGetPayload2, m.handleGetPayload2).Methods(http.MethodPost)
	r.HandleFunc(pathGetOPPayload2, m.handleOPGetPayload2).Methods(http.MethodGet)

	r.Use(mux.CORSMethodMiddleware(r))
	loggedRouter := httplogger.LoggingMiddlewareLogrus(m.log, r)
	return loggedRouter
}

// StartHTTPServer starts the HTTP server for this boost service instance
func (m *BoostService) StartHTTPServer() error {
	if m.srv != nil {
		return errServerAlreadyRunning
	}

	go m.startBidCacheCleanupTask()

	m.srv = &http.Server{
		Addr:    m.listenAddr,
		Handler: m.getRouter(),

		ReadTimeout:       time.Duration(config.ServerReadTimeoutMs) * time.Millisecond,
		ReadHeaderTimeout: time.Duration(config.ServerReadHeaderTimeoutMs) * time.Millisecond,
		WriteTimeout:      time.Duration(config.ServerWriteTimeoutMs) * time.Millisecond,
		IdleTimeout:       time.Duration(config.ServerIdleTimeoutMs) * time.Millisecond,

		MaxHeaderBytes: config.ServerMaxHeaderBytes,
	}

	err := m.srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (m *BoostService) startBidCacheCleanupTask() {
	for {
		time.Sleep(1 * time.Minute)
		m.bidsLock.Lock()
		for k, bidResp := range m.bids {
			if time.Since(bidResp.t) > 3*time.Minute {
				delete(m.bids, k)
			}
		}
		m.bidsLock.Unlock()
	}
}

func (m *BoostService) sendValidatorRegistrationsToRelayMonitors(payload []builderApiV1.SignedValidatorRegistration) {
	log := m.log.WithField("method", "sendValidatorRegistrationsToRelayMonitors").WithField("numRegistrations", len(payload))
	for _, relayMonitor := range m.relayMonitors {
		go func(relayMonitor *url.URL) {
			url := GetURI(relayMonitor, pathRegisterValidator)
			log = log.WithField("url", url)
			_, err := SendHTTPRequest(context.Background(), m.httpClientRegVal, http.MethodPost, url, "", nil, payload, nil)
			if err != nil {
				log.WithError(err).Warn("error calling registerValidator on relay monitor")
				return
			}
			log.Debug("sent validator registrations to relay monitor")
		}(relayMonitor)
	}
}

// func (m *BoostService) sendAuctionTranscriptToRelayMonitors(transcript *AuctionTranscript) {
// 	log := m.log.WithField("method", "sendAuctionTranscriptToRelayMonitors")
// 	for _, relayMonitor := range m.relayMonitors {
// 		go func(relayMonitor *url.URL) {
// 			url := GetURI(relayMonitor, pathAuctionTranscript)
// 			log := log.WithField("url", url)
// 			_, err := SendHTTPRequest(context.Background(), *http.DefaultClient, http.MethodPost, url, UserAgent(""), nil, transcript, nil)
// 			if err != nil {
// 				log.WithError(err).Warn("error sending auction transcript to relay monitor")
// 				return
// 			}
// 			log.Debug("sent auction transcript to relay monitor")
// 		}(relayMonitor)
// 	}
// }

func (m *BoostService) handleRoot(w http.ResponseWriter, _ *http.Request) {
	m.respondOK(w, nilResponse)
}

// handleStatus sends calls to the status endpoint of every relay.
// It returns OK if at least one returned OK, and returns error otherwise.
func (m *BoostService) handleStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(HeaderKeyVersion, config.Version)
	if !m.relayCheck || m.CheckRelays() > 0 {
		m.respondOK(w, nilResponse)
	} else {
		m.respondError(w, http.StatusServiceUnavailable, "all relays are unavailable")
	}
}

// handleRegisterValidator - returns 200 if at least one relay returns 200, else 502
func (m *BoostService) handleRegisterValidator(w http.ResponseWriter, req *http.Request) {
	log := m.log.WithField("method", "registerValidator")
	log.Debug("registerValidator")

	payload := []builderApiV1.SignedValidatorRegistration{}
	if err := DecodeJSON(req.Body, &payload); err != nil {
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ua := UserAgent(req.Header.Get("User-Agent"))
	log = log.WithFields(logrus.Fields{
		"numRegistrations": len(payload),
		"ua":               ua,
	})

	relayRespCh := make(chan error, len(m.relays))

	for _, relay := range m.relays {
		go func(relay RelayEntry) {
			url := relay.GetURI(pathRegisterValidator)
			log := log.WithField("url", url)

			_, err := SendHTTPRequest(context.Background(), m.httpClientRegVal, http.MethodPost, url, ua, nil, payload, nil)
			relayRespCh <- err
			if err != nil {
				log.WithError(err).Warn("error calling registerValidator on relay")
				return
			}
		}(relay)
	}

	go m.sendValidatorRegistrationsToRelayMonitors(payload)

	for i := 0; i < len(m.relays); i++ {
		respErr := <-relayRespCh
		if respErr == nil {
			m.respondOK(w, nilResponse)
			return
		}
	}

	m.respondError(w, http.StatusBadGateway, errNoSuccessfulRelayResponse.Error())
}

// handleGetHeader requests bids from the relays(Baton)
// type SEQHeaderRequest struct {
// 	Slot           uint64 `json:"slot"`
// 	NumToBTxs      int   `json:"numtobtxs,omitempty"`
// 	NumRoBChains   int   `json:"numrobchains,omitempty"`
// 	NumRoBChunkTxs int   `json:"numrobchunktxs,omitempty"`
// }

func (m *BoostService) CreateTransaction(nonce uint64, value big.Int, gasLimit uint64, gasPrice big.Int, data string) *core.Transaction {
	toAddress := common.HexToAddress(TestAddressValue)
	_, err := crypto.HexToECDSA(TestPrivateKeyValue)
	if err != nil {
	  log.Fatalf("Failed to load private key: %v", err)
	}
  
	tx := core.NewTx(&core.LegacyTx{
	  Nonce:    nonce,
	  To:       &toAddress,
	  Value:    &value,
	  Gas:      gasLimit,
	  GasPrice: &gasPrice,
	  Data:     []byte(data),
	})
  
	return tx
}

// makes txs into byte form and signs txs
func (m *BoostService) CreateTransactionAsTxBytes(nonce uint64, value big.Int, gasLimit uint64, gasPrice big.Int, data string) hexutil.Bytes {
	privateKey, err := crypto.HexToECDSA(TestPrivateKeyValue)
	if err != nil {
		log.Fatalf("Failed to load private key: %v", err)
	}

	tx := m.CreateTransaction(nonce, value, gasLimit, gasPrice, data)

	chainID := big.NewInt(3) // Ropsten
	signedTx, err := core.SignTx(tx, core.NewEIP155Signer(chainID), privateKey)
	if err != nil {
		log.Fatalf("Failed to sign transaction: %v", err)
	}

	rawTxBytes, err := signedTx.MarshalBinary()
	if err != nil {
		log.Fatalf("Failed to serialize transaction: %v", err)
	}

	return rawTxBytes
}

func (m *BoostService) CreateRandomTransaction(nonce uint64) hexutil.Bytes {
	value := big.NewInt(int64(rand.Intn(101)))
	gasLimit := rand.Intn(101)
	gasPrice := big.NewInt(int64(rand.Intn(101)))
	call := m.CreateTransactionAsTxBytes(nonce, *value, uint64(gasLimit), *gasPrice, data) 
	return call
}

func (m *BoostService) CreateRandomTransactions(nonce uint64, numTxs uint64) []hexutil.Bytes {
	var list []hexutil.Bytes
	for i := 0; i < int(numTxs); i = i+1 {
		tx := m.CreateRandomTransaction(nonce + uint64(i))
		list = append(list, tx)
	}
	return list
}


func (m *BoostService) handleGetHeader2(w http.ResponseWriter, req *http.Request) {
	//TODO: everytime this function is called, we need mock chunks
	// generate mock chunks' headers(Baton would generate headers for mock chunks) and return them back to seq

	// parsing SEQHeaderRequest
	vars := mux.Vars(req)
	slot := vars["Slot"]
	numToBTxs := vars["NumToBTxs"]
	numRoBChains := vars["NumRoBChains"]
	numRoBChunkTxs := vars["NumRoBChunkTxs"]

	// converts slot from string to uint64
	_slot, err := strconv.ParseUint(slot, 10, 64)
	if err != nil {
		m.respondError(w, http.StatusBadRequest, errInvalidSlot.Error())
		return
	}

	_numToBTxs, err := strconv.ParseUint(numToBTxs, 10, 64)
	if err != nil {
		m.respondError(w, http.StatusBadRequest, errInvalidSlot.Error())
		return
	}

	_numRoBChains, err := strconv.ParseUint(numRoBChains, 10, 64)
	if err != nil {
		m.respondError(w, http.StatusBadRequest, errInvalidSlot.Error())
		return
	}

	_numRoBChunkTxs, err := strconv.ParseUint(numRoBChunkTxs, 10, 64)
	if err != nil {
		m.respondError(w, http.StatusBadRequest, errInvalidSlot.Error())
		return
	}
	mockChunks := m.CreateRandomTransactions(nonce, _numToBTxs)

	// populated ToB chunk of SEQ block with the 1 chunk made above
	m.mockChunkToB = mockChunks

	// populated RoB chunks by numRoBChains(counter for how many RoB chunks are in SEQ block)
	// and then creates random txs for those RoB chunks
	for i := 0; i < int(_numRoBChains); i += 1 {
		mockRoBTxs := m.CreateRandomTransactions(nonce, _numRoBChunkTxs)
	}

	// sending the response to SEQ awaiting signature
	var res *SEQHeaderResponse
	m.respondOK(w, res)

	// // send
	// ua := UserAgent(req.Header.Get("User-Agent"))
	// log := m.log.WithFields(logrus.Fields{
	// 	"method":     "getHeader",
	// 	"slot":       slot,
	// 	"parentHash": parentHashHex,
	// 	"pubkey":     pubkey,
	// 	"ua":         ua,
	// })
	// log.Debug("getHeader")

	// _slot, err := strconv.ParseUint(slot, 10, 64)
	// if err != nil {
	// 	m.respondError(w, http.StatusBadRequest, errInvalidSlot.Error())
	// 	return
	// }

	// if len(pubkey) != 98 {
	// 	m.respondError(w, http.StatusBadRequest, errInvalidPubkey.Error())
	// 	return
	// }

	// if len(parentHashHex) != 66 {
	// 	m.respondError(w, http.StatusBadRequest, errInvalidHash.Error())
	// 	return
	// }

	// // Make sure we have a uid for this slot
	// m.slotUIDLock.Lock()
	// if m.slotUID.slot < _slot {
	// 	m.slotUID.slot = _slot
	// 	m.slotUID.uid = uuid.New()
	// }
	// slotUID := m.slotUID.uid
	// m.slotUIDLock.Unlock()
	// log = log.WithField("slotUID", slotUID)

	// // Log how late into the slot the request starts
	// slotStartTimestamp := m.genesisTime + _slot*config.SlotTimeSec
	// msIntoSlot := uint64(time.Now().UTC().UnixMilli()) - slotStartTimestamp*1000
	// log.WithFields(logrus.Fields{
	// 	"genesisTime": m.genesisTime,
	// 	"slotTimeSec": config.SlotTimeSec,
	// 	"msIntoSlot":  msIntoSlot,
	// }).Infof("getHeader request start - %d milliseconds into slot %d", msIntoSlot, _slot)

	// // Add request headers
	// headers := map[string]string{
	// 	HeaderKeySlotUID: slotUID.String(),
	// }

	// // Prepare relay responses
	// result := bidResp{}                           // the final response, containing the highest bid (if any)
	// relays := make(map[BlockHashHex][]RelayEntry) // relays that sent the bid for a specific blockHash

	// Call the relays
	// var mu sync.Mutex
	// var wg sync.WaitGroup
	// for _, relay := range m.relays {
	// 	wg.Add(1)
	// 	go func(relay RelayEntry) {
	// 		defer wg.Done()
	// 		// goal: get an execution payload header
	// 		// 1.) this endpoint below exists in Baton but needs implementation fixes
	// 		// 2.) pass
	// 		path := fmt.Sprintf("/eth/v1/builder/header/%s/%s/%s", slot, parentHashHex, pubkey)
	// 		url := relay.GetURI(path)
	// 		log := log.WithField("url", url)
	// 		responsePayload := new(builderSpec.VersionedSignedBuilderBid)
	// 		code, err := SendHTTPRequest(context.Background(), m.httpClientGetHeader, http.MethodGet, url, ua, headers, nil, responsePayload)
	// 		if err != nil {
	// 			log.WithError(err).Warn("error making request to relay")
	// 			return
	// 		}

	// 		if code == http.StatusNoContent {
	// 			log.Debug("no-content response")
	// 			return
	// 		}

	// 		// Skip if payload is empty
	// 		if responsePayload.IsEmpty() {
	// 			return
	// 		}

	// 		// Getting the bid info will check if there are missing fields in the response
	// 		bidInfo, err := parseBidInfo(responsePayload)
	// 		if err != nil {
	// 			log.WithError(err).Warn("error parsing bid info")
	// 			return
	// 		}

	// 		if bidInfo.blockHash == nilHash {
	// 			log.Warn("relay responded with empty block hash")
	// 			return
	// 		}

	// 		valueEth := weiBigIntToEthBigFloat(bidInfo.value.ToBig())
	// 		log = log.WithFields(logrus.Fields{
	// 			"blockNumber": bidInfo.blockNumber,
	// 			"blockHash":   bidInfo.blockHash.String(),
	// 			"txRoot":      bidInfo.txRoot.String(),
	// 			"value":       valueEth.Text('f', 18),
	// 		})

	// 		if relay.PublicKey.String() != bidInfo.pubkey.String() {
	// 			log.Errorf("bid pubkey mismatch. expected: %s - got: %s", relay.PublicKey.String(), bidInfo.pubkey.String())
	// 			return
	// 		}

	// 		// Verify the relay signature in the relay response
	// 		if !config.SkipRelaySignatureCheck {
	// 			ok, err := checkRelaySignature(responsePayload, m.builderSigningDomain, relay.PublicKey)
	// 			if err != nil {
	// 				log.WithError(err).Error("error verifying relay signature")
	// 				return
	// 			}
	// 			if !ok {
	// 				log.Error("failed to verify relay signature")
	// 				return
	// 			}
	// 		}

	// 		// Verify response coherence with proposer's input data
	// 		if bidInfo.parentHash.String() != parentHashHex {
	// 			log.WithFields(logrus.Fields{
	// 				"originalParentHash": parentHashHex,
	// 				"responseParentHash": bidInfo.parentHash.String(),
	// 			}).Error("proposer and relay parent hashes are not the same")
	// 			return
	// 		}

	// 		isZeroValue := bidInfo.value.IsZero()
	// 		isEmptyListTxRoot := bidInfo.txRoot.String() == "0x7ffe241ea60187fdb0187bfa22de35d1f9bed7ab061d9401fd47e34a54fbede1"
	// 		if isZeroValue || isEmptyListTxRoot {
	// 			log.Warn("ignoring bid with 0 value")
	// 			return
	// 		}
	// 		log.Debug("bid received")

	// 		// Skip if value (fee) is lower than the minimum bid
	// 		if bidInfo.value.CmpBig(m.relayMinBid.BigInt()) == -1 {
	// 			log.Debug("ignoring bid below min-bid value")
	// 			return
	// 		}

	// 		mu.Lock()
	// 		defer mu.Unlock()

	// 		// Remember which relays delivered which bids (multiple relays might deliver the top bid)
	// 		relays[BlockHashHex(bidInfo.blockHash.String())] = append(relays[BlockHashHex(bidInfo.blockHash.String())], relay)

	// 		// Compare the bid with already known top bid (if any)
	// 		if !result.response.IsEmpty() {
	// 			valueDiff := bidInfo.value.Cmp(result.bidInfo.value)
	// 			if valueDiff == -1 { // current bid is less profitable than already known one
	// 				return
	// 			} else if valueDiff == 0 { // current bid is equally profitable as already known one. Use hash as tiebreaker
	// 				previousBidBlockHash := result.bidInfo.blockHash
	// 				if bidInfo.blockHash.String() >= previousBidBlockHash.String() {
	// 					return
	// 				}
	// 			}
	// 		}

	// 		// Use this relay's response as mev-boost response because it's most profitable
	// 		log.Debug("new best bid")
	// 		result.response = *responsePayload
	// 		result.bidInfo = bidInfo
	// 		result.t = time.Now()
	// 	}(relay)
	// }

	// // Wait for all requests to complete...
	// wg.Wait()

	// if result.response.IsEmpty() {
	// 	log.Info("no bid received")
	// 	w.WriteHeader(http.StatusNoContent)
	// 	return
	// }

	// // Log result
	// valueEth := weiBigIntToEthBigFloat(result.bidInfo.value.ToBig())
	// result.relays = relays[BlockHashHex(result.bidInfo.blockHash.String())]
	// log.WithFields(logrus.Fields{
	// 	"blockHash":   result.bidInfo.blockHash.String(),
	// 	"blockNumber": result.bidInfo.blockNumber,
	// 	"txRoot":      result.bidInfo.txRoot.String(),
	// 	"value":       valueEth.Text('f', 18),
	// 	"relays":      strings.Join(RelayEntriesToStrings(result.relays), ", "),
	// }).Info("best bid")

	// // Remember the bid, for future logging in case of withholding
	// bidKey := bidRespKey{slot: _slot, blockHash: result.bidInfo.blockHash.String()}
	// m.bidsLock.Lock()
	// m.bids[bidKey] = result
	// m.bidsLock.Unlock()

	// // Return the bid
	// m.respondOK(w, &result.response)
}

func (m *BoostService) handleGetHeader(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	slot := vars["slot"]
	parentHashHex := vars["parent_hash"]
	pubkey := vars["pubkey"]

	ua := UserAgent(req.Header.Get("User-Agent"))
	log := m.log.WithFields(logrus.Fields{
		"method":     "getHeader",
		"slot":       slot,
		"parentHash": parentHashHex,
		"pubkey":     pubkey,
		"ua":         ua,
	})
	log.Debug("getHeader")

	_slot, err := strconv.ParseUint(slot, 10, 64)
	if err != nil {
		m.respondError(w, http.StatusBadRequest, errInvalidSlot.Error())
		return
	}

	if len(pubkey) != 98 {
		m.respondError(w, http.StatusBadRequest, errInvalidPubkey.Error())
		return
	}

	if len(parentHashHex) != 66 {
		m.respondError(w, http.StatusBadRequest, errInvalidHash.Error())
		return
	}

	// Make sure we have a uid for this slot
	m.slotUIDLock.Lock()
	if m.slotUID.slot < _slot {
		m.slotUID.slot = _slot
		m.slotUID.uid = uuid.New()
	}
	slotUID := m.slotUID.uid
	m.slotUIDLock.Unlock()
	log = log.WithField("slotUID", slotUID)

	// Log how late into the slot the request starts
	slotStartTimestamp := m.genesisTime + _slot*config.SlotTimeSec
	msIntoSlot := uint64(time.Now().UTC().UnixMilli()) - slotStartTimestamp*1000
	log.WithFields(logrus.Fields{
		"genesisTime": m.genesisTime,
		"slotTimeSec": config.SlotTimeSec,
		"msIntoSlot":  msIntoSlot,
	}).Infof("getHeader request start - %d milliseconds into slot %d", msIntoSlot, _slot)

	// Add request headers
	headers := map[string]string{
		HeaderKeySlotUID: slotUID.String(),
	}

	// Prepare relay responses
	result := bidResp{}                           // the final response, containing the highest bid (if any)
	relays := make(map[BlockHashHex][]RelayEntry) // relays that sent the bid for a specific blockHash

	// Call the relays
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, relay := range m.relays {
		wg.Add(1)
		go func(relay RelayEntry) {
			defer wg.Done()
			path := fmt.Sprintf("/eth/v1/builder/header/%s/%s/%s", slot, parentHashHex, pubkey)
			url := relay.GetURI(path)
			log := log.WithField("url", url)
			responsePayload := new(builderSpec.VersionedSignedBuilderBid)
			code, err := SendHTTPRequest(context.Background(), m.httpClientGetHeader, http.MethodGet, url, ua, headers, nil, responsePayload)
			if err != nil {
				log.WithError(err).Warn("error making request to relay")
				return
			}

			if code == http.StatusNoContent {
				log.Debug("no-content response")
				return
			}

			// Skip if payload is empty
			if responsePayload.IsEmpty() {
				return
			}

			// Getting the bid info will check if there are missing fields in the response
			bidInfo, err := parseBidInfo(responsePayload)
			if err != nil {
				log.WithError(err).Warn("error parsing bid info")
				return
			}

			if bidInfo.blockHash == nilHash {
				log.Warn("relay responded with empty block hash")
				return
			}

			valueEth := weiBigIntToEthBigFloat(bidInfo.value.ToBig())
			log = log.WithFields(logrus.Fields{
				"blockNumber": bidInfo.blockNumber,
				"blockHash":   bidInfo.blockHash.String(),
				"txRoot":      bidInfo.txRoot.String(),
				"value":       valueEth.Text('f', 18),
			})

			if relay.PublicKey.String() != bidInfo.pubkey.String() {
				log.Errorf("bid pubkey mismatch. expected: %s - got: %s", relay.PublicKey.String(), bidInfo.pubkey.String())
				return
			}

			// Verify the relay signature in the relay response
			if !config.SkipRelaySignatureCheck {
				ok, err := checkRelaySignature(responsePayload, m.builderSigningDomain, relay.PublicKey)
				if err != nil {
					log.WithError(err).Error("error verifying relay signature")
					return
				}
				if !ok {
					log.Error("failed to verify relay signature")
					return
				}
			}

			// Verify response coherence with proposer's input data
			if bidInfo.parentHash.String() != parentHashHex {
				log.WithFields(logrus.Fields{
					"originalParentHash": parentHashHex,
					"responseParentHash": bidInfo.parentHash.String(),
				}).Error("proposer and relay parent hashes are not the same")
				return
			}

			isZeroValue := bidInfo.value.IsZero()
			isEmptyListTxRoot := bidInfo.txRoot.String() == "0x7ffe241ea60187fdb0187bfa22de35d1f9bed7ab061d9401fd47e34a54fbede1"
			if isZeroValue || isEmptyListTxRoot {
				log.Warn("ignoring bid with 0 value")
				return
			}
			log.Debug("bid received")

			// Skip if value (fee) is lower than the minimum bid
			if bidInfo.value.CmpBig(m.relayMinBid.BigInt()) == -1 {
				log.Debug("ignoring bid below min-bid value")
				return
			}

			mu.Lock()
			defer mu.Unlock()

			// Remember which relays delivered which bids (multiple relays might deliver the top bid)
			relays[BlockHashHex(bidInfo.blockHash.String())] = append(relays[BlockHashHex(bidInfo.blockHash.String())], relay)

			// Compare the bid with already known top bid (if any)
			if !result.response.IsEmpty() {
				valueDiff := bidInfo.value.Cmp(result.bidInfo.value)
				if valueDiff == -1 { // current bid is less profitable than already known one
					return
				} else if valueDiff == 0 { // current bid is equally profitable as already known one. Use hash as tiebreaker
					previousBidBlockHash := result.bidInfo.blockHash
					if bidInfo.blockHash.String() >= previousBidBlockHash.String() {
						return
					}
				}
			}

			// Use this relay's response as mev-boost response because it's most profitable
			log.Debug("new best bid")
			result.response = *responsePayload
			result.bidInfo = bidInfo
			result.t = time.Now()
		}(relay)
	}

	// Wait for all requests to complete...
	wg.Wait()

	if result.response.IsEmpty() {
		log.Info("no bid received")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Log result
	valueEth := weiBigIntToEthBigFloat(result.bidInfo.value.ToBig())
	result.relays = relays[BlockHashHex(result.bidInfo.blockHash.String())]
	log.WithFields(logrus.Fields{
		"blockHash":   result.bidInfo.blockHash.String(),
		"blockNumber": result.bidInfo.blockNumber,
		"txRoot":      result.bidInfo.txRoot.String(),
		"value":       valueEth.Text('f', 18),
		"relays":      strings.Join(RelayEntriesToStrings(result.relays), ", "),
	}).Info("best bid")

	// Remember the bid, for future logging in case of withholding
	bidKey := bidRespKey{slot: _slot, blockHash: result.bidInfo.blockHash.String()}
	m.bidsLock.Lock()
	m.bids[bidKey] = result
	m.bidsLock.Unlock()

	// Return the bid
	m.respondOK(w, &result.response)
}

// note: recieve payload from Baton first, then we verify the payload and send to SEQ
func (m *BoostService) processCapellaPayload(w http.ResponseWriter, req *http.Request, log *logrus.Entry, payload *eth2ApiV1Capella.SignedBlindedBeaconBlock, body []byte) {
	if payload.Message == nil || payload.Message.Body == nil || payload.Message.Body.ExecutionPayloadHeader == nil {
		log.WithField("body", string(body)).Error("missing parts of the request payload from the beacon-node")
		m.respondError(w, http.StatusBadRequest, "missing parts of the payload")
		return
	}

	// Get the slotUID for this slot
	slotUID := ""
	m.slotUIDLock.Lock()
	if m.slotUID.slot == uint64(payload.Message.Slot) {
		slotUID = m.slotUID.uid.String()
	} else {
		log.Warnf("latest slotUID is for slot %d rather than payload slot %d", m.slotUID.slot, payload.Message.Slot)
	}
	m.slotUIDLock.Unlock()

	// Prepare logger
	ua := UserAgent(req.Header.Get("User-Agent"))
	log = log.WithFields(logrus.Fields{
		"ua":         ua,
		"slot":       payload.Message.Slot,
		"blockHash":  payload.Message.Body.ExecutionPayloadHeader.BlockHash.String(),
		"parentHash": payload.Message.Body.ExecutionPayloadHeader.ParentHash.String(),
		"slotUID":    slotUID,
	})

	// Log how late into the slot the request starts
	slotStartTimestamp := m.genesisTime + uint64(payload.Message.Slot)*config.SlotTimeSec
	msIntoSlot := uint64(time.Now().UTC().UnixMilli()) - slotStartTimestamp*1000
	log.WithFields(logrus.Fields{
		"genesisTime": m.genesisTime,
		"slotTimeSec": config.SlotTimeSec,
		"msIntoSlot":  msIntoSlot,
	}).Infof("submitBlindedBlock request start - %d milliseconds into slot %d", msIntoSlot, payload.Message.Slot)

	// Get the bid!
	bidKey := bidRespKey{slot: uint64(payload.Message.Slot), blockHash: payload.Message.Body.ExecutionPayloadHeader.BlockHash.String()}
	m.bidsLock.Lock()
	originalBid := m.bids[bidKey]
	m.bidsLock.Unlock()
	if originalBid.response.IsEmpty() {
		log.Error("no bid for this getPayload payload found. was getHeader called before?")
	} else if len(originalBid.relays) == 0 {
		log.Warn("bid found but no associated relays")
	}

	// send bid and signed block to relay monitor with eth2ApiV1Capella payload
	// go m.sendAuctionTranscriptToRelayMonitors(&AuctionTranscript{Bid: originalBid.response.Data, Acceptance: payload})

	// Add request headers
	headers := map[string]string{HeaderKeySlotUID: slotUID}

	// Prepare for requests
	var wg sync.WaitGroup
	var mu sync.Mutex
	result := new(builderApi.VersionedSubmitBlindedBlockResponse)

	// Prepare the request context, which will be cancelled after the first successful response from a relay
	requestCtx, requestCtxCancel := context.WithCancel(context.Background())
	defer requestCtxCancel()

	for _, relay := range m.relays {
		wg.Add(1)
		go func(relay RelayEntry) {
			defer wg.Done()
			url := relay.GetURI(pathGetPayload)
			log := log.WithField("url", url)
			log.Debug("calling getPayload")

			responsePayload := new(builderApi.VersionedSubmitBlindedBlockResponse)
			_, err := SendHTTPRequestWithRetries(requestCtx, m.httpClientGetPayload, http.MethodPost, url, ua, headers, payload, responsePayload, m.requestMaxRetries, log)
			if err != nil {
				if errors.Is(requestCtx.Err(), context.Canceled) {
					log.Info("request was cancelled") // this is expected, if payload has already been received by another relay
				} else {
					log.WithError(err).Error("error making request to relay")
				}
				return
			}

			if getPayloadResponseIsEmpty(responsePayload) {
				log.Error("response with empty data!")
				return
			}

			// Ensure the response blockhash matches the request
			if payload.Message.Body.ExecutionPayloadHeader.BlockHash != responsePayload.Capella.BlockHash {
				log.WithFields(logrus.Fields{
					"responseBlockHash": responsePayload.Capella.BlockHash.String(),
				}).Error("requestBlockHash does not equal responseBlockHash")
				return
			}

			// Ensure the response blockhash matches the response block
			calculatedBlockHash, err := boostUtils.ComputeBlockHash(&builderApi.VersionedExecutionPayload{
				Version: responsePayload.Version,
				Capella: responsePayload.Capella,
			}, nil)
			if err != nil {
				log.WithError(err).Error("could not calculate block hash")
			} else if responsePayload.Capella.BlockHash != calculatedBlockHash {
				log.WithFields(logrus.Fields{
					"calculatedBlockHash": calculatedBlockHash.String(),
					"responseBlockHash":   responsePayload.Capella.BlockHash.String(),
				}).Error("responseBlockHash does not equal hash calculated from response block")
			}

			// Lock before accessing the shared payload
			mu.Lock()
			defer mu.Unlock()

			if requestCtx.Err() != nil { // request has been cancelled (or deadline exceeded)
				return
			}

			// Received successful response. Now cancel other requests and return immediately
			requestCtxCancel()
			*result = *responsePayload
			log.Info("received payload from relay")
		}(relay)
	}

	// Wait for all requests to complete...
	wg.Wait()

	// If no payload has been received from relay, log loudly about withholding!
	if result.Capella == nil || result.Capella.BlockHash == nilHash {
		originRelays := RelayEntriesToStrings(originalBid.relays)
		log.WithField("relaysWithBid", strings.Join(originRelays, ", ")).Error("no payload received from relay!")
		m.respondError(w, http.StatusBadGateway, errNoSuccessfulRelayResponse.Error())
		return
	}

	m.respondOK(w, result)
}

func (m *BoostService) processDenebPayload(w http.ResponseWriter, req *http.Request, log *logrus.Entry, blindedBlock *eth2ApiV1Deneb.SignedBlindedBeaconBlock) {
	// Get the currentSlotUID for this slot
	currentSlotUID := ""
	m.slotUIDLock.Lock()
	if m.slotUID.slot == uint64(blindedBlock.Message.Slot) {
		currentSlotUID = m.slotUID.uid.String()
	} else {
		log.Warnf("latest slotUID is for slot %d rather than payload slot %d", m.slotUID.slot, blindedBlock.Message.Slot)
	}
	m.slotUIDLock.Unlock()
	//TODO new stuff here?

	// Prepare logger
	ua := UserAgent(req.Header.Get("User-Agent"))
	log = log.WithFields(logrus.Fields{
		"ua":         ua,
		"slot":       blindedBlock.Message.Slot,
		"blockHash":  blindedBlock.Message.Body.ExecutionPayloadHeader.BlockHash.String(),
		"parentHash": blindedBlock.Message.Body.ExecutionPayloadHeader.ParentHash.String(),
		"slotUID":    currentSlotUID,
	})

	// Log how late into the slot the request starts
	slotStartTimestamp := m.genesisTime + uint64(blindedBlock.Message.Slot)*config.SlotTimeSec
	msIntoSlot := uint64(time.Now().UTC().UnixMilli()) - slotStartTimestamp*1000
	log.WithFields(logrus.Fields{
		"genesisTime": m.genesisTime,
		"slotTimeSec": config.SlotTimeSec,
		"msIntoSlot":  msIntoSlot,
	}).Infof("submitBlindedBlock request start - %d milliseconds into slot %d", msIntoSlot, blindedBlock.Message.Slot)

	// Get the bid!
	bidKey := bidRespKey{slot: uint64(blindedBlock.Message.Slot), blockHash: blindedBlock.Message.Body.ExecutionPayloadHeader.BlockHash.String()}
	m.bidsLock.Lock()
	originalBid := m.bids[bidKey]
	m.bidsLock.Unlock()
	if originalBid.response.IsEmpty() {
		log.Error("no bid for this getPayload payload found, was getHeader called before?")
	} else if len(originalBid.relays) == 0 {
		log.Warn("bid found but no associated relays")
	}

	// Add request headers
	headers := map[string]string{HeaderKeySlotUID: currentSlotUID}

	// Prepare for requests
	var wg sync.WaitGroup
	var mu sync.Mutex
	result := new(builderApi.VersionedSubmitBlindedBlockResponse)

	// Prepare the request context, which will be cancelled after the first successful response from a relay
	requestCtx, requestCtxCancel := context.WithCancel(context.Background())
	defer requestCtxCancel()

	for _, relay := range m.relays {
		wg.Add(1)
		go func(relay RelayEntry) {
			defer wg.Done()
			url := relay.GetURI(pathGetPayload)
			log := log.WithField("url", url)
			log.Debug("calling getPayload")

			responsePayload := new(builderApi.VersionedSubmitBlindedBlockResponse)
			_, err := SendHTTPRequestWithRetries(requestCtx, m.httpClientGetPayload, http.MethodPost, url, ua, headers, blindedBlock, responsePayload, m.requestMaxRetries, log)
			if err != nil {
				if errors.Is(requestCtx.Err(), context.Canceled) {
					log.Info("request was cancelled") // this is expected, if payload has already been received by another relay
				} else {
					log.WithError(err).Error("error making request to relay")
				}
				return
			}

			if getPayloadResponseIsEmpty(responsePayload) {
				log.Error("response with empty data!")
				return
			}

			payload := responsePayload.Deneb.ExecutionPayload
			blobs := responsePayload.Deneb.BlobsBundle

			// Ensure the response blockhash matches the request
			if blindedBlock.Message.Body.ExecutionPayloadHeader.BlockHash != payload.BlockHash {
				log.WithFields(logrus.Fields{
					"responseBlockHash": payload.BlockHash.String(),
				}).Error("requestBlockHash does not equal responseBlockHash")
				return
			}

			commitments := blindedBlock.Message.Body.BlobKZGCommitments
			// Ensure that blobs are valid and matches the request
			if len(commitments) != len(blobs.Blobs) || len(commitments) != len(blobs.Commitments) || len(commitments) != len(blobs.Proofs) {
				log.WithFields(logrus.Fields{
					"requestBlobCommitments":  len(commitments),
					"responseBlobs":           len(blobs.Blobs),
					"responseBlobCommitments": len(blobs.Commitments),
					"responseBlobProofs":      len(blobs.Proofs),
				}).Error("block KZG commitment length does not equal responseBlobs length")
				return
			}

			for i, commitment := range commitments {
				if commitment != blobs.Commitments[i] {
					log.WithFields(logrus.Fields{
						"requestBlobCommitment":  commitment.String(),
						"responseBlobCommitment": blobs.Commitments[i].String(),
						"index":                  i,
					}).Error("requestBlobCommitment does not equal responseBlobCommitment")
					return
				}
			}

			// Lock before accessing the shared payload
			mu.Lock()
			defer mu.Unlock()

			if requestCtx.Err() != nil { // request has been cancelled (or deadline exceeded)
				return
			}

			// Received successful response. Now cancel other requests and return immediately
			requestCtxCancel()
			*result = *responsePayload
			log.Info("received payload from relay")
		}(relay)
	}

	// Wait for all requests to complete...
	wg.Wait()

	// If no payload has been received from relay, log loudly about withholding!
	if getPayloadResponseIsEmpty(result) {
		originRelays := RelayEntriesToStrings(originalBid.relays)
		log.WithField("relaysWithBid", strings.Join(originRelays, ", ")).Error("no payload received from relay!")
		m.respondError(w, http.StatusBadGateway, errNoSuccessfulRelayResponse.Error())
		return
	}

	m.respondOK(w, result)
}

func (m *BoostService) handleGetPayload(w http.ResponseWriter, req *http.Request) {
	log := m.log.WithField("method", "getPayload")
	log.Debug("getPayload request starts")

	// Read the body first, so we can log it later on error
	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.WithError(err).Error("could not read body of request from the beacon node")
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Decode the body now
	payload := new(eth2ApiV1Deneb.SignedBlindedBeaconBlock)
	if err := DecodeJSON(bytes.NewReader(body), payload); err != nil {
		log.Debug("could not decode Deneb request payload, attempting to decode body into Capella payload")
		payload := new(eth2ApiV1Capella.SignedBlindedBeaconBlock)
		if err := DecodeJSON(bytes.NewReader(body), payload); err != nil {
			log.WithError(err).WithField("body", string(body)).Error("could not decode request payload from the beacon-node (signed blinded beacon block)")
			m.respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		m.processCapellaPayload(w, req, log, payload, body)
		return
	}
	m.processDenebPayload(w, req, log, payload)
}

// note function for integration
func (m *BoostService) handleGetPayload2(w http.ResponseWriter, req *http.Request) {
	log := m.log.WithField("method", "getPayload")
	log.Debug("getPayload request starts")

	// Read the body first, so we can log it later on error
	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.WithError(err).Error("could not read body of request from the beacon node")
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// type SEQPayloadResponse struct {
	// 	Slot        uint64                        `json:"slot"`
	// 	ToBPayload  ExecutionPayload2            `json:"tobpayload"`
	// 	RoBPayloads map[string]ExecutionPayload2 `json:"robpayloads"`
	// }
	// type SEQPayloadRequest struct {
	// 	Slot                   uint64                                     `json:"slot"`
	// 	ToBBlindedBeaconBlock  AnchorSignedBlindedBeaconBlock            `json:"tobblindedbeaconblock"`
	// 	RoBBlindedBeaconBlocks map[string]AnchorSignedBlindedBeaconBlock `json:"robblindedbeaconblocks"`
	// }
	// need the new function
	var payload SEQPayloadResponse
	var payloadReq SEQPayloadRequest

	err = payloadReq.FromJSON(body)
	if err != nil {
		log.WithError(err).Error("could not deserialize body from payload request")
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	resBody, err2 := payload.ToJSON()
	if err2 != nil {
		log.WithError(err).Error("could not serialize payload response")
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	m.respondOK(w, resBody)

	// Decode the body now
	// payload := new(eth2ApiV1Deneb.SignedBlindedBeaconBlock)
	// if err := DecodeJSON(bytes.NewReader(body), payload); err != nil {
	// 	log.Debug("could not decode Deneb request payload, attempting to decode body into Capella payload")
	// 	payload := new(eth2ApiV1Capella.SignedBlindedBeaconBlock)
	// 	if err := DecodeJSON(bytes.NewReader(body), payload); err != nil {
	// 		log.WithError(err).WithField("body", string(body)).Error("could not decode request payload from the beacon-node (signed blinded beacon block)")
	// 		m.respondError(w, http.StatusBadRequest, err.Error())
	// 		return
	// 	}
	// 	m.processCapellaPayload(w, req, log, payload, body)
	// 	return
	// }
	// m.processDenebPayload(w, req, log, payload)
}

func (m *BoostService) handleOPGetPayload2(w http.ResponseWriter, req *http.Request) {
	log := m.log.WithField("method", "getPayload")
	log.Debug("getPayload request starts")

	vars := mux.Vars(req)
	parentHashHex := vars["parent_hash"]

	ua := UserAgent(req.Header.Get("User-Agent"))

	if len(parentHashHex) != 66 {
		m.respondError(w, http.StatusBadRequest, errInvalidHash.Error())
		return
	}

	// Prepare relay responses
	result := opBidResp{}                         // the final response, containing the highest bid (if any)
	relays := make(map[BlockHashHex][]RelayEntry) // relays that sent the bid for a specific blockHash

	// Call the relays
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, relay := range m.relays {
		wg.Add(1)
		go func(relay RelayEntry) {
			defer wg.Done()
			path := fmt.Sprintf("/eth/v1/builder/get_payload/%s", parentHashHex)
			url := relay.GetURI(path)
			log := log.WithField("url", url)
			responsePayload := new(OPBid)
			code, err := SendHTTPRequest(context.Background(), m.httpClientOPGetPayload, http.MethodGet, url, ua, nil, nil, responsePayload)
			if err != nil {
				log.WithError(err).Warn("error making request to relay")
				return
			}

			if code == http.StatusNoContent {
				log.Debug("no-content response")
				return
			}

			// Skip if payload is empty
			if responsePayload.IsEmpty() {
				return
			}

			// Getting the bid info will check if there are missing fields in the response
			bidInfo := bidInfo{
				blockHash:   phase0.Hash32(responsePayload.Payload.BlockHash),
				parentHash:  phase0.Hash32(responsePayload.Payload.ParentHash),
				blockNumber: uint64(responsePayload.Payload.BlockNumber),
				value:       responsePayload.Value,

				// ignored fields:
				// txRoot
				// pubkey
			}

			if phase0.Hash32(responsePayload.Payload.BlockHash) == nilHash {
				log.Warn("relay responded with empty block hash")
				return
			}

			valueEth := weiBigIntToEthBigFloat(responsePayload.Value.ToBig())
			log = log.WithFields(logrus.Fields{
				"blockNumber": bidInfo.blockNumber,
				"blockHash":   bidInfo.blockHash.String(),
				"value":       valueEth.Text('f', 18),
			})

			// if relay.PublicKey.String() != bidInfo.pubkey.String() {
			// 	log.Errorf("bid pubkey mismatch. expected: %s - got: %s", relay.PublicKey.String(), bidInfo.pubkey.String())
			// 	return
			// }

			// // Verify the relay signature in the relay response
			// if !config.SkipRelaySignatureCheck {
			// 	ok, err := checkRelaySignature(responsePayload, m.builderSigningDomain, relay.PublicKey)
			// 	if err != nil {
			// 		log.WithError(err).Error("error verifying relay signature")
			// 		return
			// 	}
			// 	if !ok {
			// 		log.Error("failed to verify relay signature")
			// 		return
			// 	}
			// }

			// Verify response coherence with proposer's input data
			if bidInfo.parentHash.String() != parentHashHex {
				log.WithFields(logrus.Fields{
					"originalParentHash": parentHashHex,
					"responseParentHash": bidInfo.parentHash.String(),
				}).Error("proposer and relay parent hashes are not the same")
				return
			}

			// isZeroValue := bidInfo.value.String() == "0"
			// isEmptyListTxRoot := bidInfo.txRoot.String() == "0x7ffe241ea60187fdb0187bfa22de35d1f9bed7ab061d9401fd47e34a54fbede1"
			// if isZeroValue || isEmptyListTxRoot {
			// 	log.Warn("ignoring bid with 0 value")
			// 	return
			// }
			log.Debug("bid received")

			// Skip if value (fee) is lower than the minimum bid
			if bidInfo.value.ToBig().Cmp(m.relayMinBid.BigInt()) == -1 {
				log.Debug("ignoring bid below min-bid value")
				return
			}

			mu.Lock()
			defer mu.Unlock()

			// Remember which relays delivered which bids (multiple relays might deliver the top bid)
			relays[BlockHashHex(bidInfo.blockHash.String())] = append(relays[BlockHashHex(bidInfo.blockHash.String())], relay)

			// Compare the bid with already known top bid (if any)
			if !result.response.IsEmpty() {
				valueDiff := bidInfo.value.Cmp(result.bidInfo.value)
				if valueDiff == -1 { // current bid is less profitable than already known one
					return
				} else if valueDiff == 0 { // current bid is equally profitable as already known one. Use hash as tiebreaker
					previousBidBlockHash := result.bidInfo.blockHash
					if bidInfo.blockHash.String() >= previousBidBlockHash.String() {
						return
					}
				}
			}

			// Use this relay's response as mev-boost response because it's most profitable
			log.Debug("new best bid")
			result.response = *responsePayload
			result.bidInfo = bidInfo
			result.t = time.Now()
		}(relay)
	}

	// Wait for all requests to complete...
	wg.Wait()

	if result.response.IsEmpty() {
		log.Info("no bid received")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Log result
	valueEth := weiBigIntToEthBigFloat(result.bidInfo.value.ToBig())
	result.relays = relays[BlockHashHex(result.bidInfo.blockHash.String())]
	log.WithFields(logrus.Fields{
		"blockHash":   result.bidInfo.blockHash.String(),
		"blockNumber": result.bidInfo.blockNumber,
		"value":       valueEth.Text('f', 18),
		"relays":      strings.Join(RelayEntriesToStrings(result.relays), ", "),
	}).Info("best bid")

	// Remember the bid, for future logging in case of withholding
	bidKey := bidRespKey{slot: result.bidInfo.blockNumber, blockHash: result.bidInfo.blockHash.String()}
	m.bidsLock.Lock()
	m.opBids[bidKey] = result
	m.bidsLock.Unlock()

	//TODO fix this part
	//TODO this needs to create a chunk
	transactions := make([]*chain.Transaction, len(result.response.Payload.Transactions))

	vm_id := uint64(1)
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, vm_id)

	chainId := "dope"

	for i, seqTx := range result.response.Payload.Transactions {
		args := &SubmitMsgTxArgs{
			ChainId:          chainId,
			NetworkID:        1337,
			SecondaryChainId: buf,
			Data:             seqTx,
		}

		t, err := m.GetSEQTransaction(*args)
		if err != nil {
			log.Info("get SEQ transaction failed")
			w.WriteHeader(http.StatusNoContent)
		}
		transactions[i] = t[0]
	}

	res := SEQResponse{
		Hght:   uint64(result.response.Payload.BlockNumber),
		Tmstmp: int64(result.response.Payload.Timestamp),
		Prnt:   ids.ID(result.response.Payload.ParentHash),
		Txs:    transactions,
	}
	//TODO make the payload here to return to HyperSDK

	// Return the bid
	// m.respondOK(w, &result.response.Payload)
	m.respondOK(w, res)
}

func (m *BoostService) handleOPGetPayload(w http.ResponseWriter, req *http.Request) {
	log := m.log.WithField("method", "getPayload")
	log.Debug("getPayload request starts")

	vars := mux.Vars(req)
	parentHashHex := vars["parent_hash"]

	ua := UserAgent(req.Header.Get("User-Agent"))

	if len(parentHashHex) != 66 {
		m.respondError(w, http.StatusBadRequest, errInvalidHash.Error())
		return
	}

	// Prepare relay responses
	result := opBidResp{}                         // the final response, containing the highest bid (if any)
	relays := make(map[BlockHashHex][]RelayEntry) // relays that sent the bid for a specific blockHash

	// Call the relays
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, relay := range m.relays {
		wg.Add(1)
		go func(relay RelayEntry) {
			defer wg.Done()
			path := fmt.Sprintf("/eth/v1/builder/get_payload/%s", parentHashHex)
			url := relay.GetURI(path)
			log := log.WithField("url", url)
			responsePayload := new(OPBid)
			code, err := SendHTTPRequest(context.Background(), m.httpClientOPGetPayload, http.MethodGet, url, ua, nil, nil, responsePayload)
			if err != nil {
				log.WithError(err).Warn("error making request to relay")
				return
			}

			if code == http.StatusNoContent {
				log.Debug("no-content response")
				return
			}

			// Skip if payload is empty
			if responsePayload.IsEmpty() {
				return
			}

			// Getting the bid info will check if there are missing fields in the response
			bidInfo := bidInfo{
				blockHash:   phase0.Hash32(responsePayload.Payload.BlockHash),
				parentHash:  phase0.Hash32(responsePayload.Payload.ParentHash),
				blockNumber: uint64(responsePayload.Payload.BlockNumber),
				value:       responsePayload.Value,

				// ignored fields:
				// txRoot
				// pubkey
			}

			if phase0.Hash32(responsePayload.Payload.BlockHash) == nilHash {
				log.Warn("relay responded with empty block hash")
				return
			}

			valueEth := weiBigIntToEthBigFloat(responsePayload.Value.ToBig())
			log = log.WithFields(logrus.Fields{
				"blockNumber": bidInfo.blockNumber,
				"blockHash":   bidInfo.blockHash.String(),
				"value":       valueEth.Text('f', 18),
			})

			// if relay.PublicKey.String() != bidInfo.pubkey.String() {
			// 	log.Errorf("bid pubkey mismatch. expected: %s - got: %s", relay.PublicKey.String(), bidInfo.pubkey.String())
			// 	return
			// }

			// // Verify the relay signature in the relay response
			// if !config.SkipRelaySignatureCheck {
			// 	ok, err := checkRelaySignature(responsePayload, m.builderSigningDomain, relay.PublicKey)
			// 	if err != nil {
			// 		log.WithError(err).Error("error verifying relay signature")
			// 		return
			// 	}
			// 	if !ok {
			// 		log.Error("failed to verify relay signature")
			// 		return
			// 	}
			// }

			// Verify response coherence with proposer's input data
			if bidInfo.parentHash.String() != parentHashHex {
				log.WithFields(logrus.Fields{
					"originalParentHash": parentHashHex,
					"responseParentHash": bidInfo.parentHash.String(),
				}).Error("proposer and relay parent hashes are not the same")
				return
			}

			// isZeroValue := bidInfo.value.String() == "0"
			// isEmptyListTxRoot := bidInfo.txRoot.String() == "0x7ffe241ea60187fdb0187bfa22de35d1f9bed7ab061d9401fd47e34a54fbede1"
			// if isZeroValue || isEmptyListTxRoot {
			// 	log.Warn("ignoring bid with 0 value")
			// 	return
			// }
			log.Debug("bid received")

			// Skip if value (fee) is lower than the minimum bid
			if bidInfo.value.ToBig().Cmp(m.relayMinBid.BigInt()) == -1 {
				log.Debug("ignoring bid below min-bid value")
				return
			}

			mu.Lock()
			defer mu.Unlock()

			// Remember which relays delivered which bids (multiple relays might deliver the top bid)
			relays[BlockHashHex(bidInfo.blockHash.String())] = append(relays[BlockHashHex(bidInfo.blockHash.String())], relay)

			// Compare the bid with already known top bid (if any)
			if !result.response.IsEmpty() {
				valueDiff := bidInfo.value.Cmp(result.bidInfo.value)
				if valueDiff == -1 { // current bid is less profitable than already known one
					return
				} else if valueDiff == 0 { // current bid is equally profitable as already known one. Use hash as tiebreaker
					previousBidBlockHash := result.bidInfo.blockHash
					if bidInfo.blockHash.String() >= previousBidBlockHash.String() {
						return
					}
				}
			}

			// Use this relay's response as mev-boost response because it's most profitable
			log.Debug("new best bid")
			result.response = *responsePayload
			result.bidInfo = bidInfo
			result.t = time.Now()
		}(relay)
	}

	// Wait for all requests to complete...
	wg.Wait()

	if result.response.IsEmpty() {
		log.Info("no bid received")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Log result
	valueEth := weiBigIntToEthBigFloat(result.bidInfo.value.ToBig())
	result.relays = relays[BlockHashHex(result.bidInfo.blockHash.String())]
	log.WithFields(logrus.Fields{
		"blockHash":   result.bidInfo.blockHash.String(),
		"blockNumber": result.bidInfo.blockNumber,
		"value":       valueEth.Text('f', 18),
		"relays":      strings.Join(RelayEntriesToStrings(result.relays), ", "),
	}).Info("best bid")

	// Remember the bid, for future logging in case of withholding
	bidKey := bidRespKey{slot: result.bidInfo.blockNumber, blockHash: result.bidInfo.blockHash.String()}
	m.bidsLock.Lock()
	m.opBids[bidKey] = result
	m.bidsLock.Unlock()

	//TODO fix this part
	//TODO this needs to create a chunk
	transactions := make([]*chain.Transaction, len(result.response.Payload.Transactions))

	vm_id := uint64(1)
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, vm_id)

	chainId := "dope"

	for i, seqTx := range result.response.Payload.Transactions {
		args := &SubmitMsgTxArgs{
			ChainId:          chainId,
			NetworkID:        1337,
			SecondaryChainId: buf,
			Data:             seqTx,
		}

		t, err := m.GetSEQTransaction(*args)
		if err != nil {
			log.Info("get SEQ transaction failed")
			w.WriteHeader(http.StatusNoContent)
		}
		transactions[i] = t[0]
	}

	res := SEQResponse{
		Hght:   uint64(result.response.Payload.BlockNumber),
		Tmstmp: int64(result.response.Payload.Timestamp),
		Prnt:   ids.ID(result.response.Payload.ParentHash),
		Txs:    transactions,
	}
	//TODO make the payload here to return to HyperSDK

	// Return the bid
	// m.respondOK(w, &result.response.Payload)
	m.respondOK(w, res)
}

type SubmitMsgTxArgs struct {
	ChainId          string `json:"chain_id"`
	NetworkID        uint32 `json:"network_id"`
	SecondaryChainId []byte `json:"secondary_chain_id"`
	Data             []byte `json:"data"`
}

type SEQResponse struct {
	Prnt   ids.ID `json:"parent"`
	Tmstmp int64  `json:"timestamp"`
	Hght   uint64 `json:"height"`

	Txs []*chain.Transaction `json:"txs"`
}

// TODO:
func (m *BoostService) GetSEQTransaction(args SubmitMsgTxArgs) ([]*chain.Transaction, error) {

	// ctx := context.Background()

	// chainId, err := ids.FromString(args.ChainId)
	// if err != nil {
	// 	return nil, err
	// }

	// endpoint := "fun"

	// if err != nil {
	// 	fmt.Errorf("error with id from string", "err", err)
	// }

	//tcli := trpc.NewJSONRPCClient(endpoint, 1337, chainId)

	// cli := rpc.NewJSONRPCClient(endpoint)

	// unitPrices, err := cli.UnitPrices(ctx, true)

	// if err != nil {
	// 	return nil, err
	// }

	// parser := m.ServerParser(ctx, args.NetworkID, chainId)

	// TODO: We omitted the previous key value. Is it needed?
	// priv, err := ed25519.GeneratePrivateKey()

	// OLD Code
	/*
	   		priv, err := ed25519.HexToKey(
	   			"323b1d8f4eed5f0da9da93071b034f2dce9d2d22692c172f3cb252a64ddfafd01b057de320297c29ad0c1f589ea216869cf1938d88c9fbd70d6748323dbf2fa7", //nolint:lll
	   		)
	       factory := auth.NewED25519Factory(priv)
	*/

	// factory := auth.NewED25519Factory(priv)
	// tpriv, err := ed25519.GeneratePrivateKey()

	// if err != nil {
	// 	return nil, err
	// }

	// TODO: The below is a hack to get compilation working.
	// trsender := tpriv.PublicKey()
	// addr := codec.Address{}
	// copy(addr[0:32], trsender[0:32])

	// action := &actions.SequencerMsg{
	// 	FromAddress: addr,
	// 	Data:        args.Data,
	// 	ChainId:     args.SecondaryChainId,
	// }

	// maxUnits, err := chain.EstimateMaxUnits(parser.Rules(time.Now().UnixMilli()), action, factory, nil)
	// if err != nil {
	// 	return nil, err
	// }
	// maxFee, err := chain.MulSum(unitPrices, maxUnits)
	// if err != nil {
	// 	return nil, err
	// }

	// now := time.Now().UnixMilli()
	// rules := parser.Rules(now)

	// base := &chain.Base{
	// 	Timestamp: utils.UnixRMilli(now, rules.GetValidityWindow()),
	// 	ChainID:   chainId,
	// 	MaxFee:    maxFee,
	// }

	// Build transaction
	// actionRegistry, authRegistry := parser.Registry()
	// tx := chain.NewTx(base, nil, action, false)
	// tx, err = tx.Sign(factory, actionRegistry, authRegistry)
	// if err != nil {
	// 	return nil, fmt.Errorf("%w: failed to sign transaction", err)
	// }

	// TODO above is new!

	// if err := tx.AuthAsyncVerify()(); err != nil {
	// 	return nil, err
	// }

	// ret := []*chain.Transaction{tx}

	// return ret, nil
	return nil, nil

}

// CheckRelays sends a request to each one of the relays previously registered to get their status
func (m *BoostService) CheckRelays() int {
	var wg sync.WaitGroup
	var numSuccessRequestsToRelay uint32

	for _, r := range m.relays {
		wg.Add(1)

		go func(relay RelayEntry) {
			defer wg.Done()
			url := relay.GetURI(pathStatus)
			log := m.log.WithField("url", url)
			log.Debug("checking relay status")

			code, err := SendHTTPRequest(context.Background(), m.httpClientGetHeader, http.MethodGet, url, "", nil, nil, nil)
			if err != nil {
				log.WithError(err).Error("relay status error - request failed")
				return
			}
			if code == http.StatusOK {
				log.Debug("relay status OK")
			} else {
				log.Errorf("relay status error - unexpected status code %d", code)
				return
			}

			// Success: increase counter and cancel all pending requests to other relays
			atomic.AddUint32(&numSuccessRequestsToRelay, 1)
		}(r)
	}

	// At the end, wait for every routine and return status according to relay's ones.
	wg.Wait()
	return int(numSuccessRequestsToRelay)
}

var _ chain.Parser = (*ServerParser)(nil)

type ServerParser struct {
	networkID uint32
	chainID   ids.ID
	genesis   *genesis.Genesis
}

func (p *ServerParser) ChainID() ids.ID {
	return p.chainID
}

func (p *ServerParser) Rules(t int64) chain.Rules {
	return p.genesis.Rules(t, p.networkID, p.chainID)
}

func (*ServerParser) Registry() (chain.ActionRegistry, chain.AuthRegistry) {
	return seqconsts.ActionRegistry, seqconsts.AuthRegistry
}

func (m *BoostService) ServerParser(ctx context.Context, networkId uint32, chainId ids.ID) chain.Parser {
	//g := j.c.Genesis()

	// The only thing this is using is the ActionRegistry and AuthRegistry so this should be fine
	return &Parser{networkId, chainId, nil}
}

var _ chain.Parser = (*Parser)(nil)

type Parser struct {
	networkID uint32
	chainID   ids.ID
	genesis   *genesis.Genesis
}

func (p *Parser) ChainID() ids.ID {
	return p.chainID
}

func (p *Parser) Rules(t int64) chain.Rules {
	return p.genesis.Rules(t, p.networkID, p.chainID)
}

func (*Parser) Registry() (chain.ActionRegistry, chain.AuthRegistry) {
	return consts.ActionRegistry, consts.AuthRegistry
}

func (m *BoostService) Parser(ctx context.Context, networkID uint32, chainId ids.ID) (chain.Parser, error) {

	return &Parser{networkID, chainId, nil}, nil
}
