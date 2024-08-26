package seq

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/AnomalyFi/hypersdk/codec"
	"github.com/AnomalyFi/hypersdk/consts"
	"github.com/AnomalyFi/hypersdk/crypto/ed25519"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/stretchr/testify/require"
)

const KEYHEX = "323b1d8f4eed5f0da9da93071b034f2dce9d2d22692c172f3cb252a64ddfafd01b057de320297c29ad0c1f589ea216869cf1938d88c9fbd70d6748323dbf2fa7"
const SEQURI = "http://127.0.0.1:9652/ext/bc/2GzsD6nCJ5nRrhFAbCF16hYPFJ3VJUUUqyyJfHXckSxkyturkH"
const CHAINID = "2GzsD6nCJ5nRrhFAbCF16hYPFJ3VJUUUqyyJfHXckSxkyturkH"
const NETWORKID = 1337

func TestGenSeqTx(t *testing.T) {
	pkBytes, err := hex.DecodeString(KEYHEX)
	require.NoError(t, err)
	pk := ed25519.PrivateKey(pkBytes)

	fmt.Printf("%+v\n", hex.EncodeToString(pk[:]))
	chainID, err := ids.FromString(CHAINID)
	require.NoError(t, err)

	cli, err := NewSeqClient(pk, SEQURI, NETWORKID, chainID)
	require.NoError(t, err)

	var txraw hexutil.Bytes
	txraw = append(txraw, 1, 2, 3)
	ethTxs := make([]hexutil.Bytes, 0)
	ethTxs = append(ethTxs, txraw)

	seqTxs, err := cli.GenerateSeqTxsFromEthRaws(context.TODO(), ethTxs)
	require.NoError(t, err)
	require.Equal(t, 1, len(seqTxs))

	tx0 := seqTxs[0]
	p := codec.NewWriter(tx0.Size(), consts.NetworkSizeLimit)
	err = tx0.Marshal(p)
	require.NoError(t, err)
	tx0Bytes := p.Bytes()

	ctx := context.TODO()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	txID, err := cli.hrpc.SubmitTx(ctx, tx0Bytes)
	require.NoError(t, err)
	fmt.Printf("tx submitted: %s\n", txID.String())
}
