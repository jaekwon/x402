package exact_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gnolang/gno/gno.land/pkg/gnoclient"
	abci "github.com/gnolang/gno/tm2/pkg/bft/abci/types"
	rpcclient "github.com/gnolang/gno/tm2/pkg/bft/rpc/client"
	ctypes "github.com/gnolang/gno/tm2/pkg/bft/rpc/core/types"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	p2pTypes "github.com/gnolang/gno/tm2/pkg/p2p/types"
	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	x402 "github.com/x402-foundation/x402/go/v2"

	"github.com/gnoverse/x402/client/exact"
)

// The chain's answers, as gno.land writes them: amino JSON, indented. The gas
// price is what onyx-1 answered on 2026-10-22; the unset one is what a chain
// whose genesis stores no price answers.
const (
	onyxGasPrice  = "{\n  \"gas\": \"1000\",\n  \"price\": \"1ugnot\"\n}"
	unsetGasPrice = "{\n  \"gas\": \"0\",\n  \"price\": \"\"\n}"

	accountJSON = `{
  "BaseAccount": {
    "address": "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5",
    "coins": "1000ugnot",
    "public_key": null,
    "account_number": "12",
    "sequence": "7"
  },
  "attributes": "0"
}`

	sessionJSON = `{
  "BaseSessionAccount": {
    "BaseAccount": {
      "address": "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq",
      "coins": "",
      "public_key": null,
      "account_number": "34",
      "sequence": "3"
    },
    "master_address": "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5",
    "expires_at": "0"
  }
}`
)

// stubRPC answers the calls the adapter makes — ABCI queries by path, and the
// node's status — and nothing else: the embedded nil Client panics on any
// other call, so a read that reaches the network some other way fails the
// test loudly. A path it has no answer for answers with no data.
type stubRPC struct {
	rpcclient.Client

	answers map[string]abci.ResponseQuery
	status  *ctypes.ResultStatus
	err     error
	paths   []string // the ABCI path each query named
}

func (s *stubRPC) ABCIQuery(ctx context.Context, path string, _ []byte) (*ctypes.ResultABCIQuery, error) {
	s.paths = append(s.paths, path)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	return &ctypes.ResultABCIQuery{Response: s.answers[path]}, nil
}

func (s *stubRPC) Status(context.Context, *int64) (*ctypes.ResultStatus, error) {
	return s.status, s.err
}

func answering(data string) abci.ResponseQuery {
	return abci.ResponseQuery{ResponseBase: abci.ResponseBase{Data: []byte(data)}}
}

func nodeOver(rpc *stubRPC) *exact.GnoclientNode {
	return exact.NewGnoclientNode(&gnoclient.Client{RPCClient: rpc})
}

// TestTheGasPriceIsReadAsTheChainWritesIt decodes a live chain's answer, the
// one a payment's fee on onyx-1 is computed from.
func TestTheGasPriceIsReadAsTheChainWritesIt(t *testing.T) {
	rpc := &stubRPC{answers: map[string]abci.ResponseQuery{"auth/gasprice": answering(onyxGasPrice)}}

	price, err := nodeOver(rpc).GasPrice(context.Background())

	require.NoError(t, err)
	assert.Equal(t, &std.GasPrice{Gas: 1000, Price: std.Coin{Denom: "ugnot", Amount: 1}}, price)
	assert.Equal(t, []string{"auth/gasprice"}, rpc.paths)
}

// TestANodeAnsweringNoGasPriceReportsNone: a chain that stored no price
// answers gas 0 at an empty price, which is no price, as are no data and JSON
// null; the scheme answers each with its unpriced fee.
func TestANodeAnsweringNoGasPriceReportsNone(t *testing.T) {
	for name, data := range map[string]string{"an unset price": unsetGasPrice, "no data": "", "null": "null"} {
		t.Run(name, func(t *testing.T) {
			rpc := &stubRPC{answers: map[string]abci.ResponseQuery{"auth/gasprice": answering(data)}}

			price, err := nodeOver(rpc).GasPrice(context.Background())

			require.NoError(t, err)
			assert.Nil(t, price)
		})
	}
}

// TestAGasPriceTheNodeCannotGiveIsAnError keeps three failures apart from a
// chain reporting no price: a node that never answered, one that refused the
// query, and an answer that is not a gas price. Pricing a payment at the
// unpriced fee on any of them would guess where the chain did not say.
func TestAGasPriceTheNodeCannotGiveIsAnError(t *testing.T) {
	unreachable := errors.New("dial tcp: connection refused")
	refused := std.UnknownRequestError{}
	cases := map[string]struct {
		rpc  *stubRPC
		want error
	}{
		"no answer": {rpc: &stubRPC{err: unreachable}, want: unreachable},
		"a refusal": {
			rpc:  &stubRPC{answers: map[string]abci.ResponseQuery{"auth/gasprice": {ResponseBase: abci.ResponseBase{Error: refused}}}},
			want: refused,
		},
		"an answer that is no gas price": {
			rpc: &stubRPC{answers: map[string]abci.ResponseQuery{"auth/gasprice": answering(`{"gas": "lots"}`)}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := nodeOver(tc.rpc).GasPrice(context.Background())

			require.Error(t, err)
			if tc.want != nil {
				assert.ErrorIs(t, err, tc.want)
			}
		})
	}
}

// TestAccountsAreReadAsTheChainWritesThem decodes an account and a session
// account in the form a gno.land node gives them.
func TestAccountsAreReadAsTheChainWritesThem(t *testing.T) {
	master := crypto.MustAddressFromString(test1Address)
	session := crypto.MustAddressFromString(payTo)
	rpc := &stubRPC{answers: map[string]abci.ResponseQuery{
		"auth/accounts/" + test1Address:                       answering(accountJSON),
		"auth/accounts/" + test1Address + "/session/" + payTo: answering(sessionJSON),
	}}
	node := nodeOver(rpc)

	acc, err := node.Account(context.Background(), master)
	require.NoError(t, err)
	assert.Equal(t, uint64(12), acc.GetAccountNumber())
	assert.Equal(t, uint64(7), acc.GetSequence())

	sess, err := node.SessionAccount(context.Background(), master, session)
	require.NoError(t, err)
	assert.Equal(t, uint64(34), sess.GetAccountNumber())
	assert.Equal(t, uint64(3), sess.GetSequence())
}

// TestAnUnfundedAccountAndARevokedSessionReadDifferently: the remedies
// differ — fund the address, or register the session again — so the errors
// do too. An account the chain holds none of is std.UnknownAddressError; a
// session the chain refuses to find is its std.SessionNotFoundError.
func TestAnUnfundedAccountAndARevokedSessionReadDifferently(t *testing.T) {
	master := crypto.MustAddressFromString(test1Address)
	session := crypto.MustAddressFromString(payTo)
	rpc := &stubRPC{answers: map[string]abci.ResponseQuery{
		"auth/accounts/" + test1Address + "/session/" + payTo: {ResponseBase: abci.ResponseBase{
			Error: std.SessionNotFoundError{}, Log: "session not found",
		}},
	}}
	node := nodeOver(rpc)

	_, err := node.Account(context.Background(), master)
	_, unknown := errors.AsType[std.UnknownAddressError](err)
	assert.True(t, unknown, "got %v", err)

	_, err = node.SessionAccount(context.Background(), master, session)
	_, notFound := errors.AsType[std.SessionNotFoundError](err)
	assert.True(t, notFound, "got %v", err)
	_, unknown = errors.AsType[std.UnknownAddressError](err)
	assert.False(t, unknown, "a revoked session is not an address to fund")
}

// TestAReadStopsWhenItsCallerGivesUp: the context reaches the RPC client, so a
// payment whose caller has given up reads nothing more.
func TestAReadStopsWhenItsCallerGivesUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rpc := &stubRPC{answers: map[string]abci.ResponseQuery{"auth/accounts/" + test1Address: answering(accountJSON)}}

	_, err := nodeOver(rpc).Account(ctx, crypto.MustAddressFromString(test1Address))

	require.ErrorIs(t, err, context.Canceled)
}

// TestAPaymentGivenUpDuringItsReadsIsNotSigned: the caller's context ends
// while the account is being read, and the payment is refused before the
// signer is asked: a signature is the one step that cannot be taken back.
func TestAPaymentGivenUpDuringItsReadsIsNotSigned(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	signer := &countingSigner{Signer: signerFrom(t, test1Mnemonic)}
	node := newFakeNode()
	node.onAccount = cancel

	_, err := exact.NewExactGnoScheme(signer, node).CreatePaymentPayload(ctx, offer(), x402.PaymentPayloadContext{})

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, signer.signs)
}

// TestTheNodeNamesTheChainItServes reads the chain id from the node's status,
// the same field the facilitator checks its own configuration against.
func TestTheNodeNamesTheChainItServes(t *testing.T) {
	rpc := &stubRPC{status: &ctypes.ResultStatus{NodeInfo: p2pTypes.NodeInfo{Network: "onyx-1"}}}

	served, err := nodeOver(rpc).NodeChainID(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "onyx-1", served)
}

// TestANodeWithNoRPCClientSaysSo: an adapter with no client to read through —
// none given, a nil adapter, or a nil RPC client inside the interface, as a
// failed rpcclient constructor returns one — reports upstream's own error
// instead of panicking inside a buyer's request.
func TestANodeWithNoRPCClientSaysSo(t *testing.T) {
	var failedDial *rpcclient.RPCClient
	cases := map[string]*exact.GnoclientNode{
		"no RPC client":       exact.NewGnoclientNode(&gnoclient.Client{}),
		"no gnoclient":        exact.NewGnoclientNode(nil),
		"a nil adapter":       nil,
		"a nil client inside": exact.NewGnoclientNode(&gnoclient.Client{RPCClient: failedDial}),
	}
	addr := address(signerFrom(t, test1Mnemonic))
	for name, node := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			_, err := node.NodeChainID(ctx)
			require.ErrorIs(t, err, gnoclient.ErrMissingRPCClient)
			_, err = node.GasPrice(ctx)
			require.ErrorIs(t, err, gnoclient.ErrMissingRPCClient)
			_, err = node.Account(ctx, addr)
			require.ErrorIs(t, err, gnoclient.ErrMissingRPCClient)
			_, err = node.SessionAccount(ctx, addr, addr)
			require.ErrorIs(t, err, gnoclient.ErrMissingRPCClient)
		})
	}
}
