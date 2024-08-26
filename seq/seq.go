package seq

import (
	"context"
	"time"

	"github.com/AnomalyFi/hypersdk/chain"
	"github.com/AnomalyFi/hypersdk/codec"
	"github.com/AnomalyFi/hypersdk/crypto/ed25519"
	"github.com/AnomalyFi/hypersdk/fees"
	hrpc "github.com/AnomalyFi/hypersdk/rpc"
	"github.com/AnomalyFi/hypersdk/utils"
	"github.com/AnomalyFi/nodekit-seq/actions"
	"github.com/AnomalyFi/nodekit-seq/auth"
	srpc "github.com/AnomalyFi/nodekit-seq/rpc"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type SeqClient struct {
	srpc   *srpc.JSONRPCClient
	hrpc   *hrpc.JSONRPCClient
	signer ed25519.PrivateKey

	parser    chain.Parser
	ChainID   ids.ID
	NetworkID uint32
}

func NewSeqClient(signer ed25519.PrivateKey, uri string, networkID uint32, chainID ids.ID) (*SeqClient, error) {
	hcli := hrpc.NewJSONRPCClient(uri)
	scli := srpc.NewJSONRPCClient(uri, networkID, chainID)
	parser, err := scli.Parser(context.TODO())
	if err != nil {
		return nil, err
	}

	return &SeqClient{
		srpc:   scli,
		hrpc:   hcli,
		signer: signer,
		parser: parser,

		ChainID:   chainID,
		NetworkID: networkID,
	}, nil
}

func (s *SeqClient) GenerateSeqTxsFromEthRaws(ctx context.Context, ethTxs []hexutil.Bytes) ([]*chain.Transaction, error) {
	parser := s.parser

	unitPrices, err := s.hrpc.UnitPrices(ctx, true)
	if err != nil {
		return nil, err
	}

	pubkey := s.signer.PublicKey()
	rsender := auth.NewED25519Address(pubkey)

	acts := make([]chain.Action, 0, len(ethTxs))
	for _, ethTx := range ethTxs {
		action := actions.SequencerMsg{
			ChainId:     s.ChainID[:],
			Data:        ethTx,
			FromAddress: rsender,
			RelayerID:   0,
		}
		acts = append(acts, &action)
	}

	now := time.Now().UnixMilli()
	authFactory := auth.NewED25519Factory(s.signer)

	actionRegistry, authRegistry := parser.Registry()

	txs := make([]*chain.Transaction, 0, len(ethTxs))
	for _, act := range acts {
		maxUnits, err := chain.EstimateUnits(parser.Rules(now), []chain.Action{act}, authFactory)
		if err != nil {
			return nil, err
		}
		maxFee, err := fees.MulSum(unitPrices, maxUnits)
		if err != nil {
			return nil, err
		}
		base := &chain.Base{
			Timestamp: utils.UnixRMilli(now, parser.Rules(now).GetValidityWindow()),
			ChainID:   s.ChainID,
			MaxFee:    maxFee,
		}

		tx := chain.NewTx(base, []chain.Action{act})
		tx, err = tx.Sign(authFactory, actionRegistry, authRegistry)
		if err != nil {
			return nil, err
		}

		txs = append(txs, tx)
	}

	return txs, nil
}

func (s *SeqClient) GenerateSeqTx(ctx context.Context, act chain.Action) (*chain.Transaction, error) {
	parser := s.parser
	unitPrices, err := s.hrpc.UnitPrices(ctx, true)
	if err != nil {
		return nil, err
	}

	now := time.Now().UnixMilli()
	authFactory := auth.NewED25519Factory(s.signer)
	actionRegistry, authRegistry := parser.Registry()

	maxUnits, err := chain.EstimateUnits(parser.Rules(now), []chain.Action{act}, authFactory)
	if err != nil {
		return nil, err
	}
	maxFee, err := fees.MulSum(unitPrices, maxUnits)
	if err != nil {
		return nil, err
	}
	base := &chain.Base{
		Timestamp: utils.UnixRMilli(now, parser.Rules(now).GetValidityWindow()),
		ChainID:   s.ChainID,
		MaxFee:    maxFee,
	}
	tx := chain.NewTx(base, []chain.Action{act})
	tx, err = tx.Sign(authFactory, actionRegistry, authRegistry)
	if err != nil {
		return nil, err
	}
	return tx, nil
}

func (s *SeqClient) GenerateTransferTx(ctx context.Context, asset ids.ID, to codec.Address, value uint64, memo []byte) (*chain.Transaction, error) {
	parser := s.parser
	act := &actions.Transfer{
		To:    to,
		Value: value,
		Asset: asset,
		Memo:  memo,
	}

	unitPrices, err := s.hrpc.UnitPrices(ctx, true)
	if err != nil {
		return nil, err
	}

	now := time.Now().UnixMilli()
	authFactory := auth.NewED25519Factory(s.signer)
	actionRegistry, authRegistry := parser.Registry()

	maxUnits, err := chain.EstimateUnits(parser.Rules(now), []chain.Action{act}, authFactory)
	if err != nil {
		return nil, err
	}
	maxFee, err := fees.MulSum(unitPrices, maxUnits)
	if err != nil {
		return nil, err
	}
	base := &chain.Base{
		Timestamp: utils.UnixRMilli(now, parser.Rules(now).GetValidityWindow()),
		ChainID:   s.ChainID,
		MaxFee:    maxFee,
	}
	tx := chain.NewTx(base, []chain.Action{act})
	tx, err = tx.Sign(authFactory, actionRegistry, authRegistry)
	if err != nil {
		return nil, err
	}
	return tx, nil

}
