package exact_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gnolang/gno/gno.land/pkg/gnoclient"
	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/bech32"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/crypto/keys"
	"github.com/gnolang/gno/tm2/pkg/sdk/bank"
	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"

	"github.com/gnoverse/x402/client/exact"
	facilitatorexact "github.com/gnoverse/x402/facilitator/exact"
	"github.com/gnoverse/x402/facilitator/exact/exacttest"
)

// TestAPaymentSatisfiesTheFacilitator is where the halves meet: a payment this
// package signs passes every check this repository's facilitator makes before it
// would broadcast one — the static rules, the signature and sequence against the
// signer's account, then a simulation — with the facilitator reading the same
// account state the payment was signed over.
func TestAPaymentSatisfiesTheFacilitator(t *testing.T) {
	signer := signerFrom(t, test1Mnemonic)
	requirements := offer()

	payload := pay(t, exact.NewExactGnoScheme(signer, newFakeNode()), requirements)

	chain := &exacttest.Node{Account: exacttest.AccountWithKey(signer.PubKey(), accountNumber, sequence)}
	verdict, err := facilitatorexact.NewFacilitator(chain, chainID).
		Verify(context.Background(), payload, requirements, nil)
	require.NoError(t, err)
	assert.True(t, verdict.IsValid)
	assert.Equal(t, test1Address, verdict.Payer)
	assert.Equal(t, 1, chain.Simulates, "a payment the account checks pass is simulated")
}

// TestUpstreamsClientWrapsThePayment checks the envelope: upstream's client
// answers the requirements it was handed with the scheme's payload, the
// version this mechanism speaks, and those requirements as the accepted offer.
func TestUpstreamsClientWrapsThePayment(t *testing.T) {
	requirements := offer()

	payload := pay(t, exact.NewExactGnoScheme(signerFrom(t, test1Mnemonic), newFakeNode()), requirements)

	assert.Equal(t, 2, payload.X402Version)
	assert.Equal(t, requirements, payload.Accepted)
	assert.Len(t, payload.Payload, 1, "the payload is one field, transaction")
	assert.NotEmpty(t, transactionOf(t, payload))
}

// TestThePaymentSendsThePriceToPayTo pins what is signed: one bank send of
// exactly the price from the payer to payTo, a fee of twice the chain's gas
// price for the gas reserved, and no memo when the offer binds none.
func TestThePaymentSendsThePriceToPayTo(t *testing.T) {
	signer := signerFrom(t, test1Mnemonic)

	tx := txOf(t, pay(t, exact.NewExactGnoScheme(signer, newFakeNode()), offer()))

	require.Len(t, tx.Msgs, 1)
	send, ok := tx.Msgs[0].(bank.MsgSend)
	require.True(t, ok, "the payment's message is %T, want bank.MsgSend", tx.Msgs[0])
	assert.Equal(t, test1Address, send.FromAddress.String())
	assert.Equal(t, payTo, send.ToAddress.String())
	assert.Equal(t, "250000ugnot", send.Amount.String())

	assert.Equal(t, int64(10_000_000), tx.Fee.GasWanted)
	assert.Equal(t, "20000ugnot", tx.Fee.GasFee.String(), "1ugnot per 1000 gas, twice over, for 10000000 gas")
	assert.Empty(t, tx.Memo)

	require.Len(t, tx.Signatures, 1)
	assert.Equal(t, signer.PubKey(), tx.Signatures[0].PubKey)
	assert.True(t, tx.Signatures[0].SessionAddr.IsZero(), "the payer's own key signs, so no session is named")
}

// TestAPaymentIsTheTransactionGnoclientSigns holds the payment to gno's own Go
// SDK. gnoclient builds a send with NewSendTx and signs it for the chain its
// signer was made for; this signs for the chain the offer names, through the
// same std.Tx.GetSignBytes. Given one chain, one account and one transaction,
// the two must agree byte for byte: a sign doc that differs by one byte makes
// every payment invalid while looking like a facilitator fault.
func TestAPaymentIsTheTransactionGnoclientSigns(t *testing.T) {
	ours := rawOf(t, pay(t, exact.NewExactGnoScheme(signerFrom(t, test1Mnemonic), newFakeNode()), offer()))

	unsigned, err := gnoclient.NewSendTx(gnoclient.BaseTxCfg{GasFee: "20000ugnot", GasWanted: 10_000_000},
		bank.MsgSend{
			FromAddress: crypto.MustAddressFromString(test1Address),
			ToAddress:   crypto.MustAddressFromString(payTo),
			Amount:      std.MustParseCoins("250000ugnot"),
		})
	require.NoError(t, err)
	gnoSigner, err := gnoclient.SignerFromBip39(test1Mnemonic, chainID, "", 0, 0)
	require.NoError(t, err)
	theirs, err := (&gnoclient.Client{Signer: gnoSigner}).SignTx(*unsigned, accountNumber, sequence)
	require.NoError(t, err)

	assert.Equal(t, amino.MustMarshal(theirs), ours)
}

// TestASessionPaymentIsTheTransactionGnoclientSigns is the same for a session
// key: gnoclient signs over the session account's number and sequence and names
// the session in the signature, and so must this.
func TestASessionPaymentIsTheTransactionGnoclientSigns(t *testing.T) {
	master := crypto.MustAddressFromString(test1Address)
	ours := rawOf(t, pay(t,
		exact.NewExactGnoScheme(signerFrom(t, sessionMnemonic), newFakeNode(), exact.WithMaster(master)), offer()))

	unsigned, err := gnoclient.NewSendTx(gnoclient.BaseTxCfg{GasFee: "20000ugnot", GasWanted: 10_000_000},
		bank.MsgSend{
			FromAddress: master,
			ToAddress:   crypto.MustAddressFromString(payTo),
			Amount:      std.MustParseCoins("250000ugnot"),
		})
	require.NoError(t, err)
	kb := keys.NewInMemory()
	_, err = kb.CreateAccount("session", sessionMnemonic, "", "", 0, 0)
	require.NoError(t, err)
	gnoSigner := gnoclient.SignerFromKeybase{Keybase: kb, Account: "session", ChainID: chainID, Master: master}
	theirs, err := (&gnoclient.Client{Signer: gnoSigner}).SignTx(*unsigned, sessionNumber, sessionSequence)
	require.NoError(t, err)

	assert.Equal(t, amino.MustMarshal(theirs), ours)
}

// TestThePaymentIsSignedForTheOffersChain shows the chain a signature commits
// to is the one the offer names, which is also the chain its node serves: the
// signature verifies over that chain's sign doc and over no other's.
func TestThePaymentIsSignedForTheOffersChain(t *testing.T) {
	signer := signerFrom(t, test1Mnemonic)
	node := newFakeNode()
	node.chainID = "test12"
	requirements := offer()
	requirements.Network = "gno:test12"

	tx := txOf(t, pay(t, exact.NewExactGnoScheme(signer, node), requirements))

	signature := tx.Signatures[0].Signature
	rendering, err := std.VerifySignaturePayload(signer.PubKey(), tx.SignDoc("test12", accountNumber, sequence), signature)
	require.NoError(t, err)
	assert.Equal(t, std.PayloadRenderingCurrent, rendering, "signed over the amount/gas fee rendering, as gnoclient signs")
	rendering, err = std.VerifySignaturePayload(signer.PubKey(), tx.SignDoc(chainID, accountNumber, sequence), signature)
	require.NoError(t, err)
	assert.Equal(t, std.PayloadRenderingNone, rendering, "a signature for test12 must not verify for another chain")
}

// TestAnOfferForAChainTheNodeDoesNotServeIsRefusedUnsigned covers the one way
// the offer's chain and the account state could disagree. The signature would
// commit to the offer's chain over the node's account number and sequence, a
// payment neither chain accepts — or, where the two accounts happen to match,
// one that pays on a chain whose state the buyer never read. Nothing is signed.
func TestAnOfferForAChainTheNodeDoesNotServeIsRefusedUnsigned(t *testing.T) {
	signer := &countingSigner{Signer: signerFrom(t, test1Mnemonic)}
	node := newFakeNode()
	node.chainID = "test12"

	_, err := exact.NewExactGnoScheme(signer, node).
		CreatePaymentPayload(context.Background(), offer(), x402.PaymentPayloadContext{})

	require.ErrorContains(t, err, "the offer is for chain dev and the node serves test12")
	assert.Zero(t, signer.signs)
	assert.Empty(t, node.accountAsked, "no account state is read for a chain the node does not serve")
}

// TestTheMemoTheOfferNamesIsSigned binds the payment to the seller's invoice:
// extra.memo travels in the signed transaction, and the facilitator, which
// refuses a payment whose memo differs, accepts it.
func TestTheMemoTheOfferNamesIsSigned(t *testing.T) {
	signer := signerFrom(t, test1Mnemonic)
	requirements := offer()
	requirements.Extra["memo"] = "inv-2026-08-1042"

	payload := pay(t, exact.NewExactGnoScheme(signer, newFakeNode()), requirements)

	assert.Equal(t, "inv-2026-08-1042", txOf(t, payload).Memo)
	_, err := facilitatorexact.NewFacilitator(
		&exacttest.Node{Account: exacttest.AccountWithKey(signer.PubKey(), accountNumber, sequence)}, chainID).
		Verify(context.Background(), payload, requirements, nil)
	require.NoError(t, err)
}

// TestTheLongestMemoTheFacilitatorTakesIsSigned is the cap's edge: a memo of
// exactly MaxMemoBytes is one the facilitator accepts, so it is signed.
func TestTheLongestMemoTheFacilitatorTakesIsSigned(t *testing.T) {
	memo := strings.Repeat("m", facilitatorexact.MaxMemoBytes)
	requirements := offer()
	requirements.Extra["memo"] = memo

	tx := txOf(t, pay(t, exact.NewExactGnoScheme(signerFrom(t, test1Mnemonic), newFakeNode()), requirements))

	assert.Equal(t, memo, tx.Memo)
}

// TestASessionPaymentMovesTheMastersCoins pays with a session key: the send
// names the master as its sender, the signature names the session, and it
// commits to the session account's own number and sequence rather than the
// master's. The facilitator, reading the session account, names the master as
// the payer.
func TestASessionPaymentMovesTheMastersCoins(t *testing.T) {
	session := signerFrom(t, sessionMnemonic)
	master := crypto.MustAddressFromString(test1Address)
	node := newFakeNode()
	requirements := offer()

	payload := pay(t, exact.NewExactGnoScheme(session, node, exact.WithMaster(master)), requirements)

	tx := txOf(t, payload)
	send := tx.Msgs[0].(bank.MsgSend)
	assert.Equal(t, master, send.FromAddress)
	assert.Equal(t, address(session), tx.Signatures[0].SessionAddr)
	assert.Equal(t, [][2]crypto.Address{{master, address(session)}}, node.sessionsAsked)
	assert.Empty(t, node.accountAsked, "the master's own account state is not what a session signs over")

	chain := &exacttest.Node{Account: exacttest.AccountWithKey(session.PubKey(), sessionNumber, sessionSequence)}
	verdict, err := facilitatorexact.NewFacilitator(chain, chainID).
		Verify(context.Background(), payload, requirements, nil)
	require.NoError(t, err)
	assert.Equal(t, test1Address, verdict.Payer)
}

// TestAnAccountTheChainDoesNotHoldIsNamed tells the buyer which address to
// fund, and keeps upstream's error for a caller that tells refusals apart by
// type.
func TestAnAccountTheChainDoesNotHoldIsNamed(t *testing.T) {
	signer := &countingSigner{Signer: signerFrom(t, test1Mnemonic)}
	node := newFakeNode()
	node.accountErr = std.ErrUnknownAddress("unknown address: " + test1Address)

	_, err := exact.NewExactGnoScheme(signer, node).
		CreatePaymentPayload(context.Background(), offer(), x402.PaymentPayloadContext{})

	require.ErrorContains(t, err, "the chain holds no account for "+test1Address+"; fund it")
	_, unknown := errors.AsType[std.UnknownAddressError](err)
	assert.True(t, unknown)
	assert.Zero(t, signer.signs)
}

// TestARevokedSessionCannotPay: a session the chain no longer holds has no
// account state to sign over, so the payment fails before it is signed.
func TestARevokedSessionCannotPay(t *testing.T) {
	session := &countingSigner{Signer: signerFrom(t, sessionMnemonic)}
	node := newFakeNode()
	node.sessionErr = std.ErrSessionNotFound("session not found")

	_, err := exact.NewExactGnoScheme(session, node, exact.WithMaster(crypto.MustAddressFromString(test1Address))).
		CreatePaymentPayload(context.Background(), offer(), x402.PaymentPayloadContext{})

	require.ErrorContains(t, err, "read session "+address(session).String()+" of "+test1Address)
	assert.Zero(t, session.signs)
}

// TestAChainThatCannotBeReadSignsNothing: every read comes before the
// signature, so a node that fails any of them leaves nothing signed, and the
// failure is the node's own error.
func TestAChainThatCannotBeReadSignsNothing(t *testing.T) {
	unreachable := errors.New("dial tcp: connection refused")
	cases := map[string]func(*fakeNode){
		"the chain it serves": func(n *fakeNode) { n.chainErr = unreachable },
		"the gas price":       func(n *fakeNode) { n.gasPriceErr = unreachable },
		"the account":         func(n *fakeNode) { n.accountErr = unreachable },
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			signer := &countingSigner{Signer: signerFrom(t, test1Mnemonic)}
			node := newFakeNode()
			fail(node)

			_, err := exact.NewExactGnoScheme(signer, node).
				CreatePaymentPayload(context.Background(), offer(), x402.PaymentPayloadContext{})

			require.ErrorIs(t, err, unreachable)
			assert.Zero(t, signer.signs)
		})
	}
}

// TestTheFeeFollowsTheChainsGasPrice pins the fee a payment offers: twice the
// chain's gas price for the 10000000 gas it reserves, rounded up, in the
// price's own denomination — the TypeScript mechanism's fee for the same chain
// state. A chain reporting no price gets a fee clear of any minimum a node sets
// on its own.
func TestTheFeeFollowsTheChainsGasPrice(t *testing.T) {
	cases := map[string]struct {
		price *std.GasPrice
		want  string
	}{
		"onyx-1's price": {
			price: &std.GasPrice{Gas: 1000, Price: std.Coin{Denom: "ugnot", Amount: 1}},
			want:  "20000ugnot",
		},
		"a price that does not divide evenly, rounded up": {
			price: &std.GasPrice{Gas: 3000, Price: std.Coin{Denom: "ugnot", Amount: 1}},
			want:  "6667ugnot",
		},
		"the highest price a payment pays at, for a fee of 1 GNOT": {
			price: &std.GasPrice{Gas: 1000, Price: std.Coin{Denom: "ugnot", Amount: 50}},
			want:  "1000000ugnot",
		},
		"no price reported": {
			price: nil,
			want:  "1000000ugnot",
		},
		"a price of nothing": {
			price: &std.GasPrice{Gas: 1000},
			want:  "1000000ugnot",
		},
		"a price for no gas": {
			price: &std.GasPrice{Price: std.Coin{Denom: "ugnot", Amount: 1}},
			want:  "1000000ugnot",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			node := newFakeNode()
			node.gasPrice = tc.price

			tx := txOf(t, pay(t, exact.NewExactGnoScheme(signerFrom(t, test1Mnemonic), node), offer()))

			assert.Equal(t, tc.want, tx.Fee.GasFee.String())
			assert.Equal(t, int64(10_000_000), tx.Fee.GasWanted)
		})
	}
}

// TestAGasPriceNoFeeCanMeetSignsNothing: a negative price, one whose fee would
// not fit in a coin, and one whose fee would pass 1 GNOT or be in a coin other
// than ugnot are answers no payment is priced against. The node reports the
// price, and a node can report anything: the cap on the fee is what keeps it
// from deciding what a payment costs.
func TestAGasPriceNoFeeCanMeetSignsNothing(t *testing.T) {
	cases := map[string]*std.GasPrice{
		"a negative amount": {Gas: 1000, Price: std.Coin{Denom: "ugnot", Amount: -1}},
		"negative gas":      {Gas: -1000, Price: std.Coin{Denom: "ugnot", Amount: 1}},
		"a fee past the range of a coin": {
			Gas: 1, Price: std.Coin{Denom: "ugnot", Amount: 1 << 62},
		},
		"a fee past 1 GNOT":     {Gas: 1000, Price: std.Coin{Denom: "ugnot", Amount: 51}},
		"a fee in another coin": {Gas: 1000, Price: std.Coin{Denom: "ufee", Amount: 5}},
	}
	for name, price := range cases {
		t.Run(name, func(t *testing.T) {
			signer := &countingSigner{Signer: signerFrom(t, test1Mnemonic)}
			node := newFakeNode()
			node.gasPrice = price

			_, err := exact.NewExactGnoScheme(signer, node).
				CreatePaymentPayload(context.Background(), offer(), x402.PaymentPayloadContext{})

			require.Error(t, err)
			assert.Zero(t, signer.signs)
		})
	}
}

// TestAPriceOverTheSpendCapIsRefusedUnsigned holds the buyer's own cap.
// Upstream's client drops an offer above it before choosing one, but a payment
// asked for directly reaches the scheme with the cap in its context, and is
// refused there.
func TestAPriceOverTheSpendCapIsRefusedUnsigned(t *testing.T) {
	signer := &countingSigner{Signer: signerFrom(t, test1Mnemonic)}
	node := newFakeNode()
	client := x402.Newx402Client().
		Register("gno:*", exact.NewExactGnoScheme(signer, node)).
		SetSpendControls(x402.SpendControls{
			AllowedAssets: []x402.SpendControlAsset{{Network: "gno:*", Asset: "ugnot", MaxAmountPerPayment: "249999"}},
		})

	_, err := client.CreatePaymentPayload(context.Background(), offer(), nil, nil)

	require.ErrorIs(t, err, exact.ErrExceedsSpendCap)
	assert.Zero(t, signer.signs)
	assert.Zero(t, node.reads, "a price over the cap is refused before the chain is read")
}

// TestAPriceAtTheSpendCapIsPaid is the cap's edge: it bounds the price, so a
// price equal to it is paid, and the fee on top is not counted against it.
func TestAPriceAtTheSpendCapIsPaid(t *testing.T) {
	scheme := exact.NewExactGnoScheme(signerFrom(t, test1Mnemonic), newFakeNode())

	_, err := scheme.CreatePaymentPayload(context.Background(), offer(),
		x402.PaymentPayloadContext{MaxAmountPerPayment: "250000"})

	require.NoError(t, err)
}

// TestASpendCapThatIsNoAmountSignsNothing: a cap the scheme cannot read caps
// nothing it can check, so nothing is paid under it.
func TestASpendCapThatIsNoAmountSignsNothing(t *testing.T) {
	signer := &countingSigner{Signer: signerFrom(t, test1Mnemonic)}

	_, err := exact.NewExactGnoScheme(signer, newFakeNode()).CreatePaymentPayload(context.Background(), offer(),
		x402.PaymentPayloadContext{MaxAmountPerPayment: "$1"})

	require.ErrorContains(t, err, "spend cap")
	assert.Zero(t, signer.signs)
}

// refusedUnsigned asserts requirements are refused as requirements, before the
// chain is read or anything is signed: they came from a seller, and a refusal
// of them must cost the buyer nothing.
func refusedUnsigned(t *testing.T, requirements types.PaymentRequirements) {
	t.Helper()
	signer := &countingSigner{Signer: signerFrom(t, test1Mnemonic)}
	node := newFakeNode()

	_, err := exact.NewExactGnoScheme(signer, node).
		CreatePaymentPayload(context.Background(), requirements, x402.PaymentPayloadContext{})

	require.ErrorIs(t, err, exact.ErrInvalidRequirements)
	assert.Zero(t, node.reads, "the chain is read only for requirements that can be paid")
	assert.Zero(t, signer.signs)
}

// TestAnOfferForNoGnoChainIsRefused: the network has to be gno:<chain-id>, with
// a chain id CAIP-2 can name, and the scheme exact.
func TestAnOfferForNoGnoChainIsRefused(t *testing.T) {
	cases := map[string]func(*types.PaymentRequirements){
		"another namespace":             func(r *types.PaymentRequirements) { r.Network = "eip155:8453" },
		"no chain id":                   func(r *types.PaymentRequirements) { r.Network = "gno:" },
		"the namespace alone":           func(r *types.PaymentRequirements) { r.Network = "gno" },
		"no namespace":                  func(r *types.PaymentRequirements) { r.Network = "dev" },
		"a network of three parts":      func(r *types.PaymentRequirements) { r.Network = "gno:dev:1" },
		"a wildcard":                    func(r *types.PaymentRequirements) { r.Network = "gno:*" },
		"a chain id CAIP-2 cannot name": func(r *types.PaymentRequirements) { r.Network = "gno:dev.1" },
		"another scheme":                func(r *types.PaymentRequirements) { r.Scheme = "upto" },
		"no scheme":                     func(r *types.PaymentRequirements) { r.Scheme = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			requirements := offer()
			mutate(&requirements)
			refusedUnsigned(t, requirements)
		})
	}
}

// TestAPriceNoCoinCarriesIsRefused: the amount and the asset become one coin
// string, so each has to be what tm2 parses as exactly that coin.
func TestAPriceNoCoinCarriesIsRefused(t *testing.T) {
	cases := map[string]func(*types.PaymentRequirements){
		"an asset in upper case":          func(r *types.PaymentRequirements) { r.Asset = "UGNOT" },
		"an asset too short":              func(r *types.PaymentRequirements) { r.Asset = "ug" },
		"an asset led by a digit":         func(r *types.PaymentRequirements) { r.Asset = "0ugnot" },
		"an asset past the longest":       func(r *types.PaymentRequirements) { r.Asset = "u" + strings.Repeat("g", 274) },
		"an asset naming two coins":       func(r *types.PaymentRequirements) { r.Asset = "ugnot,1foo" },
		"no asset":                        func(r *types.PaymentRequirements) { r.Asset = "" },
		"an amount of zero":               func(r *types.PaymentRequirements) { r.Amount = "0" },
		"an amount led by a zero":         func(r *types.PaymentRequirements) { r.Amount = "0250000" },
		"a negative amount":               func(r *types.PaymentRequirements) { r.Amount = "-250000" },
		"a signed amount":                 func(r *types.PaymentRequirements) { r.Amount = "+250000" },
		"a fractional amount":             func(r *types.PaymentRequirements) { r.Amount = "0.25" },
		"a dollar amount":                 func(r *types.PaymentRequirements) { r.Amount = "$0.25" },
		"no amount":                       func(r *types.PaymentRequirements) { r.Amount = "" },
		"an amount past the range of one": func(r *types.PaymentRequirements) { r.Amount = "9223372036854775808" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			requirements := offer()
			mutate(&requirements)
			refusedUnsigned(t, requirements)
		})
	}
}

// TestAPayeeThatIsNoGnoAddressIsRefused: payTo becomes the send's recipient,
// and the facilitator matches the recipient's canonical form against payTo as
// written, so anything else is refused before a payment to it exists.
func TestAPayeeThatIsNoGnoAddressIsRefused(t *testing.T) {
	recipient := crypto.MustAddressFromString(payTo)
	otherPrefix, err := bech32.ConvertAndEncode("cosmos", recipient[:])
	require.NoError(t, err)
	cases := map[string]string{
		"no payTo":               "",
		"not bech32":             "g1nope",
		"another chain's prefix": otherPrefix,
		"an EVM address":         "0x209693Bc6afc0C5328bA36FaF03C514EF312287C",
		"in upper case":          strings.ToUpper(payTo),
		"a bad checksum":         payTo[:len(payTo)-1] + "x",
		"the zero address":       crypto.Address{}.String(),
	}
	for name, to := range cases {
		t.Run(name, func(t *testing.T) {
			requirements := offer()
			requirements.PayTo = to
			refusedUnsigned(t, requirements)
		})
	}
}

// TestAnUnusableMemoIsRefused: a memo the offer names but that cannot be
// signed in — not a string, or over the facilitator's cap — is a malformed
// offer, not an absent memo.
func TestAnUnusableMemoIsRefused(t *testing.T) {
	cases := map[string]any{
		"a number":          42,
		"null":              nil,
		"an object":         map[string]any{"id": "inv-1"},
		"one byte too long": strings.Repeat("m", facilitatorexact.MaxMemoBytes+1),
	}
	for name, memo := range cases {
		t.Run(name, func(t *testing.T) {
			requirements := offer()
			requirements.Extra["memo"] = memo
			refusedUnsigned(t, requirements)
		})
	}
}

// TestTermsTheMechanismDoesNotImplementAreRefused pins §6.1's reserved keys
// from the buyer's side, as the facilitator pins them from its own: an offer
// naming a transfer method or a flow this mechanism does not implement is not
// signed, while omitting either key, setting it to null as upstream's SDKs read
// it, or naming the mechanism's own, is the default and paid.
func TestTermsTheMechanismDoesNotImplementAreRefused(t *testing.T) {
	cases := map[string]struct {
		extra   map[string]any
		refused bool
	}{
		"the mechanism's own transfer method": {extra: map[string]any{"assetTransferMethod": facilitatorexact.AssetTransferMethod}},
		"a null transfer method":              {extra: map[string]any{"assetTransferMethod": nil}},
		"the authorization flow":              {extra: map[string]any{"paymentFlow": "authorization"}},
		"a null flow":                         {extra: map[string]any{"paymentFlow": nil}},
		"another transfer method":             {extra: map[string]any{"assetTransferMethod": "eip3009"}, refused: true},
		"a transfer method that is no string": {extra: map[string]any{"assetTransferMethod": 1}, refused: true},
		"another flow":                        {extra: map[string]any{"paymentFlow": "upfront"}, refused: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			requirements := offer()
			requirements.Extra = tc.extra
			if tc.refused {
				refusedUnsigned(t, requirements)
				return
			}
			_, err := exact.NewExactGnoScheme(signerFrom(t, test1Mnemonic), newFakeNode()).
				CreatePaymentPayload(context.Background(), requirements, x402.PaymentPayloadContext{})
			require.NoError(t, err)
		})
	}
}

// TestAnIncompleteSchemeSignsNothing: a scheme made without a signer or a node
// reports it rather than panicking inside a buyer's request.
func TestAnIncompleteSchemeSignsNothing(t *testing.T) {
	_, err := exact.NewExactGnoScheme(nil, newFakeNode()).
		CreatePaymentPayload(context.Background(), offer(), x402.PaymentPayloadContext{})
	require.ErrorContains(t, err, "needs both a signer and a node")

	_, err = exact.NewExactGnoScheme(signerFrom(t, test1Mnemonic), nil).
		CreatePaymentPayload(context.Background(), offer(), x402.PaymentPayloadContext{})
	require.ErrorContains(t, err, "needs both a signer and a node")
}

// TestANodeAnsweringANilAccountIsAnError: a Node is an interface others
// implement, and one that answers a nil account with no error is reported,
// not followed into a panic inside the buyer's request.
func TestANodeAnsweringANilAccountIsAnError(t *testing.T) {
	signer := &countingSigner{Signer: signerFrom(t, test1Mnemonic)}
	node := newFakeNode()
	node.account = (*std.BaseAccount)(nil)

	_, err := exact.NewExactGnoScheme(signer, node).
		CreatePaymentPayload(context.Background(), offer(), x402.PaymentPayloadContext{})

	require.ErrorContains(t, err, "the node answered with no account")
	assert.Zero(t, signer.signs)
}
