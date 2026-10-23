package exact_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x402-foundation/x402/go/v2/types"

	"github.com/gnoverse/x402/client/exact"
)

// The TypeScript fixtures are payments the TypeScript mechanism in client/src
// signed, for interopOffer against the account state and gas price newFakeNode
// reports: testdata/tsclient/sign.mjs holds the same numbers. They are committed,
// so this test needs no JS toolchain; regenerating them needs npm and is only for
// changing what either mechanism signs.
//
//go:generate ./testdata/tsclient/regen.sh
const (
	tsMasterFixture  = "tsclient_master_payment.b64"
	tsSessionFixture = "tsclient_session_payment.b64"
)

// interopOffer is the offer the TypeScript fixtures answer: the fixture offer
// with a memo bound, so the memo's path through both mechanisms is compared too.
func interopOffer() types.PaymentRequirements {
	requirements := offer()
	requirements.Extra["memo"] = "x402-interop"
	return requirements
}

// fixture is a committed base64 transaction, decoded to the bytes it carries.
func fixture(t *testing.T, path ...string) []byte {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join(path...))
	require.NoError(t, err, "fixture missing — regenerate with go generate ./client/exact")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	require.NoError(t, err)
	return raw
}

// decode is a transaction's bytes, decoded as the facilitator and the chain
// decode them.
func decode(t *testing.T, raw []byte) std.Tx {
	t.Helper()
	var tx std.Tx
	require.NoError(t, amino.Unmarshal(raw, &tx))
	return tx
}

// TestAPaymentIsTheTypeScriptMechanismsByteForByte is the cross-language
// check: for the same offer, key and chain state, the Go and TypeScript
// mechanisms sign the same transaction, down to the signature's bytes. tm2's
// secp256k1 signatures are deterministic, so equal signatures mean equal sign
// docs — the same chain id, account number, sequence, fee, message and memo,
// rendered the same way.
//
// The bytes as sent differ in one place, so the second assertion compares
// through amino. amino writes a signature's session_addr even when it is the
// zero address, as that address's bech32 string, where the TypeScript encoding
// leaves the field out. Both decode to the same transaction, the signature
// covers neither, and the facilitator relays whichever bytes the buyer sent;
// only the transaction's hash differs, and a buyer reads that from the
// settlement. Should amino come to skip the zero address, as std.Signature's
// own comment says it does, the assertion still holds.
func TestAPaymentIsTheTypeScriptMechanismsByteForByte(t *testing.T) {
	theirs := fixture(t, "testdata", tsMasterFixture)

	ours := rawOf(t, pay(t, exact.NewExactGnoScheme(signerFrom(t, test1Mnemonic), newFakeNode()), interopOffer()))

	assert.Equal(t, decode(t, theirs).Signatures[0].Signature, decode(t, ours).Signatures[0].Signature)
	assert.Equal(t, amino.MustMarshal(decode(t, theirs)), ours,
		"the payment is the TypeScript one, as amino encodes a transaction")
}

// TestASessionPaymentIsTheTypeScriptMechanismsByteForByte is the same for a
// session key, and there the bytes are identical as sent: the signature names
// the session, so both encodings write session_addr.
func TestASessionPaymentIsTheTypeScriptMechanismsByteForByte(t *testing.T) {
	theirs := fixture(t, "testdata", tsSessionFixture)
	master := crypto.MustAddressFromString(test1Address)

	ours := rawOf(t, pay(t,
		exact.NewExactGnoScheme(signerFrom(t, sessionMnemonic), newFakeNode(), exact.WithMaster(master)), interopOffer()))

	assert.Equal(t, theirs, ours)
}

// TestKeybaseSignerSignsWhatTheJSSDKSigned checks the signing key against the
// facilitator's interop fixture, a payment @gnolang/gno-js-client 2 signed for
// test1 at account 0, sequence 0 on chain dev. That SDK signed the fee in its
// gas_wanted/gas_fee rendering, which the chain still accepts. Signing that same
// sign doc through KeybaseSigner reproduces the SDK's signature byte for byte,
// so the key a mnemonic yields and what it signs agree across the two SDKs.
func TestKeybaseSignerSignsWhatTheJSSDKSigned(t *testing.T) {
	signed := decode(t, fixture(t, "..", "..", "facilitator", "exact", "testdata", "gnojs_signed_send.b64"))
	signer := signerFrom(t, test1Mnemonic)

	signBytes, err := signed.GetSignBytesLegacy(chainID, 0, 0)
	require.NoError(t, err)
	signature, err := signer.Sign(signBytes)
	require.NoError(t, err)

	require.Len(t, signed.Signatures, 1)
	assert.Equal(t, signed.Signatures[0].PubKey, signer.PubKey())
	assert.Equal(t, signed.Signatures[0].Signature, signature)
}
