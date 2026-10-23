package exact_test

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/crypto/keys"
	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/require"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"

	"github.com/gnoverse/x402/client/exact"
)

const (
	// test1Mnemonic is gno's well-known test1 key. Nothing here is broadcast, so
	// the key only has to be the same one on both sides of each comparison.
	test1Mnemonic = "source bonus chronic canvas draft south burst lottery vacant surface solve popular case indicate oppose farm nothing bullet exhibit title speed wink action roast"

	// test1Address is the address test1Mnemonic derives.
	test1Address = "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5"

	// sessionMnemonic is a session key, public like test1's and never funded: the
	// e2e scenarios register it as a session of a master account.
	sessionMnemonic = "dance carry grunt uphold cloth wire usage usage clock reduce surprise page destroy reopen multiply away defense promote person web cigar immense armed enough"

	// chainID is the chain the fixture node serves and the fixture offer names.
	chainID = "dev"

	// payTo is the address the fixture offer pays.
	payTo = "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq"
)

// The account state the fixture node reports: the master's own account, and
// the session account under it, numbered apart so a signature over the wrong
// pair shows. Both are the TypeScript mechanism's fake provider's.
const (
	accountNumber   = 12
	sequence        = 7
	sessionNumber   = 34
	sessionSequence = 3
)

// offer is the fixture requirements: 250000ugnot to payTo on gno:dev.
func offer() types.PaymentRequirements {
	return types.PaymentRequirements{
		Scheme:            "exact",
		Network:           "gno:" + chainID,
		Asset:             "ugnot",
		Amount:            "250000",
		PayTo:             payTo,
		MaxTimeoutSeconds: 300,
		Extra:             map[string]any{"areFeesSponsored": false},
	}
}

// signerFrom is the Signer for the first account mnemonic derives, held in a
// keybase in memory the way a buyer holding a mnemonic would hold it.
func signerFrom(t *testing.T, mnemonic string) exact.Signer {
	t.Helper()
	kb := keys.NewInMemory()
	_, err := kb.CreateAccount("buyer", mnemonic, "", "", 0, 0)
	require.NoError(t, err)
	signer, err := exact.KeybaseSigner(kb, "buyer", "")
	require.NoError(t, err)
	return signer
}

// address is the address a signer signs as.
func address(signer exact.Signer) crypto.Address {
	return signer.PubKey().Address()
}

// countingSigner counts the signatures made through it, so a refusal can be
// shown to have signed nothing.
type countingSigner struct {
	exact.Signer
	signs int
}

func (s *countingSigner) Sign(msg []byte) ([]byte, error) {
	s.signs++
	return s.Signer.Sign(msg)
}

// fakeNode is a scripted chain. Each field sets what the matching read answers,
// and reads counts the reads that reached it.
type fakeNode struct {
	chainID  string
	account  std.Account
	session  std.Account
	gasPrice *std.GasPrice

	chainErr, accountErr, sessionErr, gasPriceErr error

	// onAccount runs during each Account read, when set.
	onAccount func()

	reads         int
	accountAsked  []crypto.Address    // the address each Account read named
	sessionsAsked [][2]crypto.Address // master and session, for each SessionAccount read
}

var _ exact.Node = (*fakeNode)(nil)

// newFakeNode is a node serving chainID at onyx-1's gas price, 1ugnot per 1000
// gas, holding the fixture account state for any address that asks.
func newFakeNode() *fakeNode {
	return &fakeNode{
		chainID:  chainID,
		account:  &std.BaseAccount{AccountNumber: accountNumber, Sequence: sequence},
		session:  &std.BaseAccount{AccountNumber: sessionNumber, Sequence: sessionSequence},
		gasPrice: &std.GasPrice{Gas: 1000, Price: std.Coin{Denom: "ugnot", Amount: 1}},
	}
}

func (n *fakeNode) NodeChainID(context.Context) (string, error) {
	n.reads++
	return n.chainID, n.chainErr
}

func (n *fakeNode) Account(_ context.Context, addr crypto.Address) (std.Account, error) {
	n.reads++
	if n.onAccount != nil {
		n.onAccount()
	}
	n.accountAsked = append(n.accountAsked, addr)
	return n.account, n.accountErr
}

func (n *fakeNode) SessionAccount(_ context.Context, master, session crypto.Address) (std.Account, error) {
	n.reads++
	n.sessionsAsked = append(n.sessionsAsked, [2]crypto.Address{master, session})
	return n.session, n.sessionErr
}

func (n *fakeNode) GasPrice(context.Context) (*std.GasPrice, error) {
	n.reads++
	return n.gasPrice, n.gasPriceErr
}

// buyer is upstream's client with scheme registered for every gno chain and
// ugnot admitted by its spend controls, uncapped: a stock buyer of gno.
func buyer(scheme *exact.ExactGnoScheme) *x402.X402Client {
	return x402.Newx402Client().
		Register("gno:*", scheme).
		SetSpendControls(x402.SpendControls{
			AllowedAssets: []x402.SpendControlAsset{{Network: "gno:*", Asset: "ugnot"}},
		})
}

// pay is the payment a stock buyer makes with scheme for requirements: the
// envelope upstream's client builds around the scheme's payload.
func pay(t *testing.T, scheme *exact.ExactGnoScheme, requirements types.PaymentRequirements) types.PaymentPayload {
	t.Helper()
	payload, err := buyer(scheme).CreatePaymentPayload(context.Background(), requirements, nil, nil)
	require.NoError(t, err)
	return payload
}

// transactionOf is the transaction field of a payload, base64 as carried.
func transactionOf(t *testing.T, payload types.PaymentPayload) string {
	t.Helper()
	transaction, ok := payload.Payload["transaction"].(string)
	require.True(t, ok, "payload transaction is %T, want string", payload.Payload["transaction"])
	return transaction
}

// rawOf is the signed transaction a payload carries, as the bytes a
// facilitator broadcasts.
func rawOf(t *testing.T, payload types.PaymentPayload) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(transactionOf(t, payload))
	require.NoError(t, err)
	return raw
}

// txOf is the signed transaction a payload carries, decoded.
func txOf(t *testing.T, payload types.PaymentPayload) std.Tx {
	t.Helper()
	var tx std.Tx
	require.NoError(t, amino.Unmarshal(rawOf(t, payload), &tx))
	return tx
}
