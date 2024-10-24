package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	seqconsts "github.com/AnomalyFi/nodekit-seq/consts"
	"github.com/ava-labs/avalanchego/ids"
	"golang.org/x/exp/rand"

	"github.com/AnomalyFi/hypersdk/chain"
	"github.com/AnomalyFi/nodekit-seq/genesis"

	"github.com/AnomalyFi/anchor/config"
	builderApiV1 "github.com/attestantio/go-builder-client/api/v1"
	builderSpec "github.com/attestantio/go-builder-client/spec"
	eth2ApiV1Bellatrix "github.com/attestantio/go-eth2-client/api/v1/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	fbls "github.com/flashbots/go-boost-utils/bls"
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
	nilHash     = phase0.Hash32{}
	nilHash2    = common.Hash{}
	nilResponse = struct{}{}
	data        string
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

// AnchorServiceOpts provides all available options for use with NewAnchorService
type AnchorServiceOpts struct {
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

	MockMode bool
}

// AnchorService - the mev-boost service
type AnchorService struct {
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

	bids     map[bidRespKey]bidResp // keeping track of bids, to log the originating relay on withholding
	bidsLock sync.Mutex

	slotUID     *slotUID
	slotUIDLock sync.Mutex

	// Below used only for testing
	mockMode bool

	// SEQ client
	// not created when mock mode enabled
	// seqCli *seq.SeqClient
}

// go program calls this on boot up automatically
func init() {
	rand.Seed(uint64(time.Now().UnixNano()))
}

// NewAnchorService created a new AnchorService
func NewAnchorService(opts AnchorServiceOpts) (*AnchorService, error) {
	if !opts.MockMode && len(opts.Relays) == 0 {
		return nil, errNoRelays
	}

	builderSigningDomain, err := ComputeDomain(ssz.DomainTypeAppBuilder, opts.GenesisForkVersionHex, phase0.Root{}.String())
	if err != nil {
		return nil, err
	}

	// seqSiginingKeyBytes, err := hex.DecodeString(config.SeqSigningKey)
	// if err != nil {
	// 	return nil, err
	// }
	// seqSigningKey := ed25519.PrivateKey(seqSiginingKeyBytes)
	// seqChainID, err := ids.FromString(config.SeqChainID)
	// if err != nil {
	// 	return nil, err
	// }

	// only enable this at mock mode, seq isn't a dependecy of Anchor
	// var seqCli *seq.SeqClient = nil
	// if opts.MockMode {
	// 	seqCli, err = seq.NewSeqClient(seqSigningKey, config.SeqURI, uint32(config.SeqNetworkID), seqChainID)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// }

	return &AnchorService{
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
		mockMode:          opts.MockMode,

		// seqCli: seqCli,
	}, nil
}

func (m *AnchorService) respondError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp := httpErrorResp{code, message}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		m.log.WithField("response", resp).WithError(err).Error("Couldn't write error response")
		http.Error(w, "", http.StatusInternalServerError)
	}
}

// note
func (m *AnchorService) respondOK(w http.ResponseWriter, response any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		m.log.WithField("response", response).WithError(err).Error("Couldn't write OK response")
		http.Error(w, "", http.StatusInternalServerError)
	}
}

func (m *AnchorService) getRouter() http.Handler {
	r := mux.NewRouter()
	r.HandleFunc("/", m.handleRoot)
	r.HandleFunc("/livez", m.handleLivez).Methods(http.MethodGet)

	r.HandleFunc(pathStatus, m.handleStatus).Methods(http.MethodGet)
	r.HandleFunc(pathRegisterValidator, m.handleRegisterValidator).Methods(http.MethodPost)

	r.HandleFunc(pathGetHeader, m.handleGetHeader).Methods(http.MethodGet)
	r.HandleFunc(pathGetPayload, m.handleGetPayload).Methods(http.MethodPost)

	// These are mock handlers for the SEQ-Anchor interface. This will be stubbed in for now.
	// r.HandleFunc(pathGetHeader2, m.handleGetHeader2).Methods(http.MethodGet)
	// r.HandleFunc(pathGetPayload2, m.handleGetPayload2).Methods(http.MethodPost)

	r.Use(mux.CORSMethodMiddleware(r))
	loggedRouter := httplogger.LoggingMiddlewareLogrus(m.log, r)
	return loggedRouter
}

// StartHTTPServer starts the HTTP server for this boost service instance
func (m *AnchorService) StartHTTPServer() error {
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

func (m *AnchorService) startBidCacheCleanupTask() {
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

func (m *AnchorService) sendValidatorRegistrationsToRelayMonitors(payload []builderApiV1.SignedValidatorRegistration) {
	log := m.log.WithField("method", "sendValidatorRegistrationsToRelayMonitors").WithField("numRegistrations", len(payload))
	for _, relayMonitor := range m.relayMonitors {
		go func(relayMonitor *url.URL) {
			uri := GetURI(relayMonitor, pathRegisterValidator)
			log = log.WithField("uri", uri)
			_, err := SendHTTPRequest(context.Background(), m.httpClientRegVal, http.MethodPost, uri, "", nil, payload, nil)
			if err != nil {
				log.WithError(err).Warn("error calling registerValidator on relay monitor")
				return
			}
			log.Debug("sent validator registrations to relay monitor")
		}(relayMonitor)
	}
}

// func (m *AnchorService) sendAuctionTranscriptToRelayMonitors(transcript *AuctionTranscript) {
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

func (m *AnchorService) handleRoot(w http.ResponseWriter, _ *http.Request) {
	m.respondOK(w, nilResponse)
}

func (m *AnchorService) handleLivez(w http.ResponseWriter, _ *http.Request) {
	m.respondMsg(w, http.StatusOK, "live")
}

func (m *AnchorService) respondMsg(w http.ResponseWriter, code int, msg string) {
	m.respond(w, code, HTTPMessageResp{msg})
}

func (m *AnchorService) respond(w http.ResponseWriter, code int, response any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if response == nil {
		return
	}

	// write the json response
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "", http.StatusInternalServerError)
	}
}

// handleStatus sends calls to the status endpoint of every relay.
// It returns OK if at least one returned OK, and returns error otherwise.
func (m *AnchorService) handleStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(HeaderKeyVersion, config.Version)
	if !m.relayCheck || m.CheckRelays() > 0 {
		m.respondOK(w, nilResponse)
	} else {
		m.respondError(w, http.StatusServiceUnavailable, "all relays are unavailable")
	}
}

// handleRegisterValidator - returns 200 if at least one relay returns 200, else 502
func (m *AnchorService) handleRegisterValidator(w http.ResponseWriter, req *http.Request) {
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
			uri := relay.GetURI(pathRegisterValidator)
			log := log.WithField("uri", uri)

			_, err := SendHTTPRequest(context.Background(), m.httpClientRegVal, http.MethodPost, uri, ua, nil, payload, nil)
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

// Original
func (m *AnchorService) handleGetHeader(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	slot := vars["slot"]
	parentHashStr := vars["parent_hash"]
	pubkey := vars["pubkey"]

	ua := UserAgent(req.Header.Get("User-Agent"))
	log := m.log.WithFields(logrus.Fields{
		"method":     "getHeader",
		"slot":       slot,
		"parentHash": parentHashStr,
		"pubkey":     pubkey,
		"ua":         ua,
	})

	_slot, err := strconv.ParseUint(slot, 10, 64)
	if err != nil {
		m.respondError(w, http.StatusBadRequest, errInvalidSlot.Error())
		return
	}

	pkBytes, err := hexutil.Decode(pubkey)
	if err != nil {
		m.respondError(w, http.StatusBadRequest, fmt.Sprintf("unable to decode pubkey as hex: %s", err))
		return
	}

	if len(pkBytes) != 48 {
		m.respondError(w, http.StatusBadRequest, errInvalidPubkey.Error())
		return
	}

	if _, err := ids.FromString(parentHashStr); err != nil {
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
	batonResponse := new(AnchorGetHeaderResponse)
	relays := make(map[BlockHashHex][]RelayEntry) // relays that sent the bid for a specific blockHash
	var responseIsGood atomic.Bool
	var chunksNotGood atomic.Bool

	// Forward the request to Baton. For now, there will be a single instance of Baton.
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, relay := range m.relays {
		wg.Add(1)

		// TODO: Since we know there is only a single Baton instance. We could probably cut out the goroutine.
		go func(relay RelayEntry) {
			defer wg.Done()
			path := fmt.Sprintf("/eth/v1/builder/header/%s/%s/%s", slot, parentHashStr, pubkey)
			url := relay.GetURI(path)
			log := log.WithField("url", url)

			localResponse := new(AnchorGetHeaderResponse)
			code, err := SendHTTPRequest(context.Background(), m.httpClientGetHeader, http.MethodGet, uri, ua, headers, nil, localResponse)
			if err != nil {
				log.WithError(err).Warn("error making request to relay")
				return
			}

			if code == http.StatusNoContent {
				log.Debug("no-content response")
				return
			}

			// Skip if payload is empty
			if localResponse.IsEmpty() {
				log.Warn("baton responded with empty header block")
				return
			}

			hasToB := localResponse.ExecHeaders.ToBHash != nil
			if hasToB && *localResponse.ExecHeaders.ToBHash.Header == nilHash2 {
				log.Warn("baton responded with empty tob block hash")
				return
			}

			var tobValue uint64
			if hasToB {
				tobValue = localResponse.ExecHeaders.ToBHash.Value.Uint64()
			}

			var robValues string
			for chainID, robChunk := range localResponse.ExecHeaders.RoBHashes {
				robValues = robValues + "[" + chainID + ":" + robChunk.Value.String() + "], "
			}

			log = log.WithFields(logrus.Fields{
				"slot":       localResponse.BlockInfo.Slot,
				"tob_value":  tobValue,
				"rob_values": robValues,
			})

			// TODO(added by chan): this following won't work as proposerPubkey belongs to SEQ
			// TODO: Add proposer pub key to Baton responses
			// relayPubKey := relay.PublicKey
			// reqPubKey := batonResponse.BlockInfo.ProposerPubkey.Bytes()
			// if relayPubKey != reqPubKey {
			// 	log.Errorf("bid pubkey mismatch. expected: %s - got: %s", relay.PublicKey.String(), batonResponse.BlockInfo.ProposerPubkey.String())
			// 	w.WriteHeader(http.StatusBadRequest)
			// 	return
			// }

			// The below checks that the message came from Baton by verifying message signature against Baton's public key.
			if !config.SkipRelaySignatureCheck {
				relayBlsPubKey, err := fbls.PublicKeyFromBytes(relay.PublicKey[:])
				if err != nil {
					log.WithError(err).Error("relay public key could not be converted to bls pubkey")
					return
				}

				ok, err := VerifyHeaderSignature(localResponse, *relayBlsPubKey)
				if err != nil {
					log.WithError(err).Error("error verifying relay signature")
					return
				}
				if !ok {
					log.Error("failed to verify relay signature")
					return
				}
			}

			// filter out invalid chunks
			relayMinBid := m.relayMinBid.BigInt()
			if hasToB && !VerifyHeader(localResponse.ExecHeaders.ToBHash, relayMinBid, log) {
				localResponse.ExecHeaders.ToBHash = nil
				log.Info("filtering out tob block")
				chunksNotGood.Store(true)
			}

			for chainID, robChunk := range localResponse.ExecHeaders.RoBHashes {
				if !VerifyHeader(robChunk, relayMinBid, log) {
					delete(localResponse.ExecHeaders.RoBHashes, chainID)
					log.Info("filtering out rob block for chain id: ", chainID)
					chunksNotGood.Store(true)
				}
			}

			mu.Lock()
			defer mu.Unlock()

			if localResponse.ExecHeaders.ToBHash != nil {
				relays[BlockHashHex(localResponse.ExecHeaders.ToBHash.BlockHash)] = append(relays[BlockHashHex(localResponse.ExecHeaders.ToBHash.BlockHash)], relay)
			}

			// make local response visible to main handler thread
			batonResponse = localResponse
			responseIsGood.Store(true)
		}(relay)

		// only process one iteration
		break
	}

	// After waiting here, we should
	wg.Wait()

	if batonResponse.IsEmpty() {
		log.Info("no seq header response had no valid chunks")

		if chunksNotGood.Load() {
			// In this case, chunks were filtered out because they did not pass verification, and response is now empty.
			w.WriteHeader(http.StatusBadRequest)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}

	// TODO: Verify bid key cache usage
	// Remember the bid, for future logging in case of withholding
	bidKey := bidRespKey{slot: _slot, blockHash: batonResponse.ParentHash.String()}
	m.bidsLock.Lock()
	bidResp := bidResp{
		t:       time.Now(),
		bidInfo: batonResponse,
	}

	m.bids[bidKey] = bidResp
	m.bidsLock.Unlock()

	// Return the bid
	m.respondOK(w, batonResponse)
}

/*
// Mock implementation
func (m *AnchorService) handleGetHeader2(w http.ResponseWriter, req *http.Request) {
	//TODO: everytime this function is called, we need mock chunks
	// generate mock chunks' headers(Baton would generate headers for mock chunks) and return them back to seq

	// parsing SEQHeaderRequest
	vars := mux.Vars(req)

	slot := vars["slot"]
	_slot, err := strconv.ParseUint(slot, 10, 64)
	fmt.Printf("slot: %s\n", slot)
	if err != nil {
		m.respondError(w, http.StatusBadRequest, errInvalidSlot.Error())
		return
	}

	// Optional param, numToBTxs indicates the number of txs in the ToB chunk
	numToBTxsToken, ok := vars["numtobtxs"]
	numToBTxs := uint64(1)
	if ok {
		_numToBTxs, err := strconv.ParseUint(numToBTxsToken, 10, 32)
		if err != nil {
			m.respondError(w, http.StatusBadRequest, errInvalidToBTxs.Error())
			return
		}
		numToBTxs = _numToBTxs
	}

	// Optional param, NumRoBChains indicates the number of chunks in the RoB
	numRoBChainsToken, ok := vars["numrobchains"]
	numRoBChains := uint64(1)
	if ok {
		_numRoBChains, err := strconv.ParseUint(numRoBChainsToken, 10, 32)
		if err != nil {
			m.respondError(w, http.StatusBadRequest, errInvalidRoBChains.Error())
			return
		}
		numRoBChains = _numRoBChains
	}

	// Optional param, NumRoBChunkTxs indicates the number of chunk
	numRoBChunkTxsToken, ok := vars["numrobchunktxs"]
	numRoBChunkTxs := uint64(1)
	if ok {
		_numRoBChunkTxs, err := strconv.ParseUint(numRoBChunkTxsToken, 10, 32)
		if err != nil {
			m.respondError(w, http.StatusBadRequest, errInvalidRoBChunkTxs.Error())
			return
		}
		numRoBChunkTxs = _numRoBChunkTxs
	}

	m.mockExpectedSlot = _slot
	// populated ToB chunk of SEQ block with the 1 chunk made above
	m.mockChunkToB = CreateRandomTransactions(m.getNextMockNonce(), numToBTxs)

	// populated RoB chunks by numRoBChains(counter for how many RoB chunks are in SEQ block)
	// and then creates random txs for those RoB chunks
	m.mockChunkRoB = make(map[string][]hexutil.Bytes)
	for i := 0; i < int(numRoBChains); i++ {
		mockRoBTxs := CreateRandomTransactions(m.incrNextMockNonce(numRoBChunkTxs), numRoBChunkTxs)
		chainID := "chain_" + strconv.Itoa(i)
		m.mockChunkRoB[chainID] = mockRoBTxs
	}

	// sending the response to SEQ awaiting signature
	res := NewSEQHeaderResponse(_slot)

	// Note for mocking this is just a random header we send back to SEQ, and, while we expect them to sign
	// it, we don't do any additional checking on the signature. Possibly might change in the future.
	tobHash := PopulateRandomHash32()
	res.ToBHash = &tobHash

	// generate a random header hash  per RoB chunk
	for k, v := range m.mockChunkRoB {
		if len(v) == 0 {
			log.Fatal("Zero-sized chunk ended up in RoB with key: " + k)
		}

		chunkHash := PopulateRandomHash32()
		res.RoBHashes[k] = chunkHash
	}

	m.respondOK(w, res)
}
*/

// original implementation
func (m *AnchorService) handleGetPayload(w http.ResponseWriter, req *http.Request) {
	log := m.log.WithField("method", "getPayload")
	log.Debug("getPayload request starts")

	// Read the body first, so we can log it later on error
	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.WithError(err).Error("could not read body of request from the validator")
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	payloadReq := AnchorGetPayloadRequest{}
	err = json.Unmarshal(body, &payloadReq)
	if err != nil {
		log.WithError(err).Error("could not read body of request in getPayload")
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get the currentSlotUID for this slot
	currentSlotUID := ""
	m.slotUIDLock.Lock()
	if m.slotUID.slot == payloadReq.Slot {
		currentSlotUID = m.slotUID.uid.String()
	} else {
		log.Warnf("latest slotUID is for slot %d rather than payload slot %d", m.slotUID.slot, payloadReq.Slot)
	}
	m.slotUIDLock.Unlock()

	// Prepare logger
	ua := UserAgent(req.Header.Get("User-Agent"))
	slot := payloadReq.Slot
	log = log.WithFields(logrus.Fields{
		"ua":         ua,
		"slot":       slot,
		"parentHash": payloadReq.ParentHash,
		"slotUID":    currentSlotUID,
	})

	// Log how late into the slot the request starts
	slotStartTimestamp := m.genesisTime + slot*config.SlotTimeSec
	msIntoSlot := uint64(time.Now().UTC().UnixMilli()) - slotStartTimestamp*1000
	log.WithFields(logrus.Fields{
		"genesisTime": m.genesisTime,
		"slotTimeSec": config.SlotTimeSec,
		"msIntoSlot":  msIntoSlot,
	}).Infof("submitBlindedBlock request start - %d milliseconds into slot %d", msIntoSlot, slot)

	// Get the bid!
	bidKey := bidRespKey{slot: slot, blockHash: payloadReq.ParentHash}
	m.bidsLock.Lock()
	originalBid := m.bids[bidKey]
	m.bidsLock.Unlock()
	if originalBid.bidInfo == nil || originalBid.bidInfo.IsEmpty() {
		log.Error("no bid for this getPayload payload found, was getHeader called before?")
	} else if len(originalBid.relays) == 0 {
		log.Warn("bid found but no associated relays")
	}

	// Add request headers
	headers := map[string]string{HeaderKeySlotUID: currentSlotUID}

	// Prepare for requests
	var wg sync.WaitGroup
	var mu sync.Mutex
	var result *AnchorGetPayloadResponse

	// Prepare the request context, which will be cancelled after the first successful response from a relay
	requestCtx, requestCtxCancel := context.WithCancel(context.Background())
	defer requestCtxCancel()

	// Since there is only one Baton instance, we can stop after processing the first iteration.
	// If ever, we have more than one Baton instance, this might have to change.
	for _, relay := range m.relays {
		wg.Add(1)

		// TODO: Since we know there is only a single Baton instance. We could probably cut out the goroutine.
		go func(relay RelayEntry) {
			defer wg.Done()
			uri := relay.GetURI(pathGetPayload)
			log := log.WithField("uri", uri)
			log.Debug("calling getPayload")

			localResult := NewAnchorGetPayloadResponse(uint64(0), true)

			_, err := SendHTTPRequestWithRetries(
				requestCtx,
				m.httpClientGetPayload,
				http.MethodPost,
				uri,
				ua,
				headers,
				payloadReq,
				&localResult,
				m.requestMaxRetries,
				log)
			if err != nil {
				if errors.Is(requestCtx.Err(), context.Canceled) {
					log.Info("request was cancelled") // this is expected, if payload has already been received by another relay
				} else {
					log.WithError(err).Error("error making request to relay")
				}
				return
			}

			if localResult.IsEmpty() {
				log.Error("response with empty data!")
				return
			}

			// TODO: Do we need something like this? Then we need to add block hash to the response.
			/*
			   // Ensure the response blockhash matches the request
			   if blindedBlock.Message.Body.ExecutionPayloadHeader.BlockHash != payload.BlockHash {
			     log.WithFields(logrus.Fields{
			       "responseBlockHash": payload.BlockHash.String(),
			     }).Error("requestBlockHash does not equal responseBlockHash")
			     return
			   }
			*/

			// The below checks that the message came from Baton by verifying message signature against Baton's public key.
			if !config.SkipRelaySignatureCheck {
				relayBlsPubKey, err := fbls.PublicKeyFromBytes(relay.PublicKey[:])
				if err != nil {
					log.WithError(err).Error("relay public key could not be converted to bls pubkey")
					return
				}

				ok, err := VerifyPayloadSignature(&localResult, *relayBlsPubKey)
				if err != nil {
					log.WithError(err).Error("error verifying relay signature")
					return
				}
				if !ok {
					log.Error("failed to verify relay signature")
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
			result = &localResult
			log.Info("received payload from relay")
		}(relay)

		// process only first relay instance
		break
	}

	// Wait for all requests to complete...
	wg.Wait()

	// If no payload has been received from relay, log loudly about withholding!
	if result == nil || result.IsEmpty() {
		originRelays := RelayEntriesToStrings(originalBid.relays)
		log.WithField("relaysWithBid", strings.Join(originRelays, ", ")).Error("no payload received from baton!")
		m.respondError(w, http.StatusBadGateway, errNoSuccessfulRelayResponse.Error())
		return
	}

	m.respondOK(w, result)
}

/*
// Mock custom handler. Note this is a true mock. It does no checking of the info
// received from SEQ and just sends the payload back.
func (m *AnchorService) handleGetPayload2(w http.ResponseWriter, req *http.Request) {
	log := m.log.WithField("method", "getPayload")
	log.Debug("getPayload request starts")

	// Read the body first, so we can log it later on error
	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.WithError(err).Error("could not read body of request from the beacon node")
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Note we don't verify any of the signatures here for now since it is mocked. Might change later.
	var payloadReq SEQPayloadRequest
	err = payloadReq.FromJSON(body)
	if err != nil {
		log.WithError(err).Error("could not deserialize body from res request")
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	if payloadReq.Slot != m.mockExpectedSlot {
		errorMsg := "received unexpected slot in GetPayload request"
		log.WithError(err).Error(errorMsg)
		m.respondError(w, http.StatusBadRequest, errorMsg)
	}

	res := NewAnchorGetPayloadResponse(payloadReq.Slot)
	res.Slot = payloadReq.Slot

	if !m.mockMode {
		seqTxs, err := m.seqCli.GenerateSeqTxsFromEthRaws(context.TODO(), m.mockChunkToB)
		if err != nil {
			log.WithError(err).Error("could not generate seq txs")
			m.respondError(w, http.StatusBadRequest, err.Error())
			return
		}

		txsRaw, err := chain.MarshalTxs(seqTxs)
		if err != nil {
			log.WithError(err).Error("could not marshal seq txs")
			m.respondError(w, http.StatusBadRequest, err.Error())
			return
		}

		res.ToBPayload.Transactions = txsRaw

		for k, v := range m.mockChunkRoB {
			seqTxs, err := m.seqCli.GenerateSeqTxsFromEthRaws(context.TODO(), v)
			if err != nil {
				log.WithError(err).Error("could not generate seq txs")
				m.respondError(w, http.StatusBadRequest, err.Error())
				return
			}

			txsRaw, err := chain.MarshalTxs(seqTxs)
			if err != nil {
				log.WithError(err).Error("could not marshal seq txs")
				m.respondError(w, http.StatusBadRequest, err.Error())
				return
			}

			res.RoBPayloads[k] = ExecutionPayload2{
				Slot:         m.mockExpectedSlot,
				Transactions: txsRaw,
			}
		}
	}

	m.respondOK(w, res)
}
*/

// CheckRelays sends a request to each one of the relays previously registered to get their status
func (m *AnchorService) CheckRelays() int {
	var wg sync.WaitGroup
	var numSuccessRequestsToRelay uint32

	for _, r := range m.relays {
		wg.Add(1)

		go func(relay RelayEntry) {
			defer wg.Done()
			uri := relay.GetURI(pathStatus)
			log := m.log.WithField("uri", uri)
			log.Debug("checking relay status")

			code, err := SendHTTPRequest(context.Background(), m.httpClientGetHeader, http.MethodGet, uri, "", nil, nil, nil)
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

func (m *AnchorService) ServerParser(_ context.Context, networkID uint32, chainID ids.ID) chain.Parser {
	// The only thing this is using is the ActionRegistry and AuthRegistry so this should be fine
	return &Parser{networkID, chainID, nil}
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
	return seqconsts.ActionRegistry, seqconsts.AuthRegistry
}

func (m *AnchorService) Parser(_ context.Context, networkID uint32, chainID ids.ID) (chain.Parser, error) {
	return &Parser{networkID, chainID, nil}, nil
}
