package exact_test

import (
	"context"
	"testing"

	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/crypto/multisig"
	"github.com/gnolang/gno/tm2/pkg/sdk/bank"
	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	x402 "github.com/x402-foundation/x402/go/v2"

	"github.com/gnoverse/x402/client/exact"
	facilitatorexact "github.com/gnoverse/x402/facilitator/exact"
	"github.com/gnoverse/x402/facilitator/exact/exacttest"
)

// TestAPaymentSignedElsewhereIsTheOneTheSchemeSigns: Prepare, a signature
// made outside the library over SignBytes, and Payload together produce
// exactly the payload CreatePaymentPayload produces in one call. A buyer
// whose key lives in gnokey or on a device loses nothing by signing it there.
func TestAPaymentSignedElsewhereIsTheOneTheSchemeSigns(t *testing.T) {
	signer := signerFrom(t, test1Mnemonic)
	requirements := interopOffer()

	inOneCall, err := exact.NewExactGnoScheme(signer, newFakeNode()).
		CreatePaymentPayload(context.Background(), requirements, x402.PaymentPayloadContext{})
	require.NoError(t, err)

	payment, err := exact.Prepare(context.Background(), newFakeNode(), signer.PubKey(), crypto.Address{}, requirements, "")
	require.NoError(t, err)
	signBytes, err := payment.SignBytes()
	require.NoError(t, err)
	signature, err := signer.Sign(signBytes)
	require.NoError(t, err)
	inTwoSteps, err := payment.Payload(signature)
	require.NoError(t, err)

	assert.Equal(t, inOneCall, inTwoSteps)
}

// TestAPreparedPaymentShowsWhatItPays: what Prepare hands back is enough to
// show a person the payment before anything is signed — the price, the payee,
// the fee and the memo — and names what the signature must commit to.
func TestAPreparedPaymentShowsWhatItPays(t *testing.T) {
	signer := signerFrom(t, test1Mnemonic)

	payment, err := exact.Prepare(context.Background(), newFakeNode(), signer.PubKey(), crypto.Address{}, interopOffer(), "")
	require.NoError(t, err)

	require.Len(t, payment.Tx.Msgs, 1)
	send := payment.Tx.Msgs[0].(bank.MsgSend)
	assert.Equal(t, test1Address, send.FromAddress.String())
	assert.Equal(t, payTo, send.ToAddress.String())
	assert.Equal(t, "250000ugnot", send.Amount.String())
	assert.Equal(t, "20000ugnot", payment.Tx.Fee.GasFee.String())
	assert.Equal(t, "x402-interop", payment.Tx.Memo)
	assert.Empty(t, payment.Tx.Signatures, "nothing is signed yet")

	assert.Equal(t, chainID, payment.ChainID)
	assert.Equal(t, uint64(accountNumber), payment.AccountNumber)
	assert.Equal(t, uint64(sequence), payment.Sequence)
	assert.Equal(t, signer.PubKey(), payment.PubKey)
	assert.True(t, payment.SessionAddr.IsZero())
}

// TestAPreparedSessionPaymentNamesTheSession: as a session, the payment moves
// the master's coins and commits to the session account's number and sequence,
// and the signature it is completed with names the session.
func TestAPreparedSessionPaymentNamesTheSession(t *testing.T) {
	session := signerFrom(t, sessionMnemonic)
	master := crypto.MustAddressFromString(test1Address)

	payment, err := exact.Prepare(context.Background(), newFakeNode(), session.PubKey(), master, offer(), "")
	require.NoError(t, err)

	assert.Equal(t, master, payment.Tx.Msgs[0].(bank.MsgSend).FromAddress)
	assert.Equal(t, uint64(sessionNumber), payment.AccountNumber)
	assert.Equal(t, uint64(sessionSequence), payment.Sequence)
	assert.Equal(t, address(session), payment.SessionAddr)

	signBytes, err := payment.SignBytes()
	require.NoError(t, err)
	signature, err := session.Sign(signBytes)
	require.NoError(t, err)
	payload, err := payment.Payload(signature)
	require.NoError(t, err)
	assert.Equal(t, address(session), txOf(t, payload).Signatures[0].SessionAddr)
}

// TestPayloadRefusesASignatureOverAnythingElse: a signature that is not the
// payment key's over this payment's sign doc is refused before it becomes a
// payload, whatever else it is valid for.
func TestPayloadRefusesASignatureOverAnythingElse(t *testing.T) {
	signer := signerFrom(t, test1Mnemonic)
	payment, err := exact.Prepare(context.Background(), newFakeNode(), signer.PubKey(), crypto.Address{}, offer(), "")
	require.NoError(t, err)

	cases := map[string]func() []byte{
		"another key": func() []byte {
			signBytes, err := payment.SignBytes()
			require.NoError(t, err)
			signature, err := signerFrom(t, sessionMnemonic).Sign(signBytes)
			require.NoError(t, err)
			return signature
		},
		"another chain": func() []byte {
			return signOver(t, signer, payment.Tx, "test12", payment.AccountNumber, payment.Sequence)
		},
		"a spent sequence": func() []byte {
			return signOver(t, signer, payment.Tx, payment.ChainID, payment.AccountNumber, payment.Sequence-1)
		},
		"another transaction": func() []byte {
			other := payment.Tx
			other.Memo = "something else"
			return signOver(t, signer, other, payment.ChainID, payment.AccountNumber, payment.Sequence)
		},
		"nothing": func() []byte { return nil },
	}
	for name, sign := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := payment.Payload(sign())
			require.ErrorContains(t, err, "the signature does not verify")
		})
	}
}

// TestPayloadTakesTheLegacyRendering: the chain verifies a signature over
// either rendering of the sign doc, so a signer that renders the fee as
// gas_wanted/gas_fee, as gno-js-client 2 and older gnokey do, pays too, and the
// facilitator accepts what it signed.
func TestPayloadTakesTheLegacyRendering(t *testing.T) {
	signer := signerFrom(t, test1Mnemonic)
	requirements := offer()
	payment, err := exact.Prepare(context.Background(), newFakeNode(), signer.PubKey(), crypto.Address{}, requirements, "")
	require.NoError(t, err)

	legacy, err := payment.Tx.GetSignBytesLegacy(payment.ChainID, payment.AccountNumber, payment.Sequence)
	require.NoError(t, err)
	signature, err := signer.Sign(legacy)
	require.NoError(t, err)
	payload, err := payment.Payload(signature)
	require.NoError(t, err)

	payload.Accepted = requirements
	chain := &exacttest.Node{Account: exacttest.AccountWithKey(signer.PubKey(), accountNumber, sequence)}
	_, err = facilitatorexact.NewFacilitator(chain, chainID).Verify(context.Background(), payload, requirements, nil)
	require.NoError(t, err)
}

// TestPrepareRefusesWhatCreatePaymentPayloadRefuses: the first half holds the
// same rails as the whole — the requirements, the cap and the node's chain —
// and signs nothing, having nothing to sign with.
func TestPrepareRefusesWhatCreatePaymentPayloadRefuses(t *testing.T) {
	pub := signerFrom(t, test1Mnemonic).PubKey()
	ctx := context.Background()

	bad := offer()
	bad.Amount = "0"
	_, err := exact.Prepare(ctx, newFakeNode(), pub, crypto.Address{}, bad, "")
	require.ErrorIs(t, err, exact.ErrInvalidRequirements)

	_, err = exact.Prepare(ctx, newFakeNode(), pub, crypto.Address{}, offer(), "249999")
	require.ErrorIs(t, err, exact.ErrExceedsSpendCap)

	elsewhere := newFakeNode()
	elsewhere.chainID = "test12"
	_, err = exact.Prepare(ctx, elsewhere, pub, crypto.Address{}, offer(), "")
	require.ErrorContains(t, err, "the offer is for chain dev and the node serves test12")

	_, err = exact.Prepare(ctx, nil, pub, crypto.Address{}, offer(), "")
	require.ErrorContains(t, err, "needs a node")
	_, err = exact.Prepare(ctx, newFakeNode(), nil, crypto.Address{}, offer(), "")
	require.ErrorContains(t, err, "no public key")
}

// signOver is signer's signature over tx's sign doc for the chain, account
// number and sequence given.
func signOver(t *testing.T, signer exact.Signer, tx std.Tx, chain string, number, seq uint64) []byte {
	t.Helper()
	signBytes, err := tx.GetSignBytes(chain, number, seq)
	require.NoError(t, err)
	signature, err := signer.Sign(signBytes)
	require.NoError(t, err)
	return signature
}

// TestAFeeOverTheCeilingIsNamed: a node quoting a gas price that would make
// the fee more than 1 GNOT is refused as ErrExcessiveFee, naming the price, and
// nothing is signed.
func TestAFeeOverTheCeilingIsNamed(t *testing.T) {
	node := newFakeNode()
	node.gasPrice = &std.GasPrice{Gas: 20_000_000, Price: std.Coin{Denom: "ugnot", Amount: 999_700_000}}

	_, err := exact.Prepare(context.Background(), node, signerFrom(t, test1Mnemonic).PubKey(), crypto.Address{}, offer(), "250000")

	require.ErrorIs(t, err, exact.ErrExcessiveFee)
	require.ErrorContains(t, err, "999700000ugnot per 20000000 gas")
}

// TestAMultisigKeyCannotPay: the facilitator takes one signature from one
// signer, so a payment a threshold key signs could never settle; it is not
// built.
func TestAMultisigKeyCannotPay(t *testing.T) {
	pub := signerFrom(t, test1Mnemonic).PubKey()
	shared := multisig.NewPubKeyMultisigThreshold(1, []crypto.PubKey{pub})

	_, err := exact.Prepare(context.Background(), newFakeNode(), shared, crypto.Address{}, offer(), "")

	require.ErrorContains(t, err, "a multisig key cannot pay")
}

// TestPayloadRefusesAKeyThatIsNotThePayers: a payment whose PubKey was changed
// after Prepare, and signed by that other key, is refused before it becomes a
// payload, as the chain would refuse it: the payer's own account, or the
// session account, names the only key that may sign.
func TestPayloadRefusesAKeyThatIsNotThePayers(t *testing.T) {
	payment, err := exact.Prepare(context.Background(), newFakeNode(), signerFrom(t, test1Mnemonic).PubKey(), crypto.Address{}, offer(), "")
	require.NoError(t, err)
	other := signerFrom(t, sessionMnemonic)
	payment.PubKey = other.PubKey()
	signBytes, err := payment.SignBytes()
	require.NoError(t, err)
	signature, err := other.Sign(signBytes)
	require.NoError(t, err)

	_, err = payment.Payload(signature)

	require.ErrorContains(t, err, "is not the key of "+test1Address)
}
