package exact_test

import (
	"sync"
	"testing"

	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/crypto/keys"
	"github.com/gnolang/gno/tm2/pkg/crypto/multisig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gnoverse/x402/client/exact"
)

// TestKeybaseSignerSignsWithTheKeyItNames: the signer's public key is the
// keybase's, found by name or by address, and what it signs verifies against
// that key.
func TestKeybaseSignerSignsWithTheKeyItNames(t *testing.T) {
	kb := keys.NewInMemory()
	info, err := kb.CreateAccount("buyer", test1Mnemonic, "", "", 0, 0)
	require.NoError(t, err)

	for _, name := range []string{"buyer", test1Address} {
		t.Run(name, func(t *testing.T) {
			signer, err := exact.KeybaseSigner(kb, name, "")
			require.NoError(t, err)

			signature, err := signer.Sign([]byte("sign bytes"))
			require.NoError(t, err)
			assert.Equal(t, info.GetPubKey(), signer.PubKey())
			assert.True(t, signer.PubKey().VerifyBytes([]byte("sign bytes"), signature))
		})
	}
}

// TestKeybaseSignerRefusesAKeyTheKeybaseDoesNotHold: a missing key is an
// error when the signer is made, before any payment is attempted with it.
func TestKeybaseSignerRefusesAKeyTheKeybaseDoesNotHold(t *testing.T) {
	_, err := exact.KeybaseSigner(keys.NewInMemory(), "buyer", "")

	require.Error(t, err)
}

// TestKeybaseSignerSignsNothingWithTheWrongPassword: a key kept encrypted
// signs only with its password, so the first payment made with the wrong one
// fails rather than producing a signature.
func TestKeybaseSignerSignsNothingWithTheWrongPassword(t *testing.T) {
	kb := keys.NewInMemory()
	_, err := kb.CreateAccount("buyer", test1Mnemonic, "", "right", 0, 0)
	require.NoError(t, err)

	wrong, err := exact.KeybaseSigner(kb, "buyer", "wrong")
	require.NoError(t, err)
	_, err = wrong.Sign([]byte("sign bytes"))
	require.Error(t, err)

	right, err := exact.KeybaseSigner(kb, "buyer", "right")
	require.NoError(t, err)
	_, err = right.Sign([]byte("sign bytes"))
	require.NoError(t, err)
}

// TestKeybaseSignerRefusesAKeyThatCannotSign: a keybase may hold only a key's
// public half, an offline key or a multisig key. Neither can ever sign, so the
// signer refuses it when it is made, rather than let a payment read the chain
// and fail after, or tell the buyer to fund an address it cannot pay from.
func TestKeybaseSignerRefusesAKeyThatCannotSign(t *testing.T) {
	pub := signerFrom(t, test1Mnemonic).PubKey()
	kb := keys.NewInMemory()
	_, err := kb.CreateOffline("watch-only", pub)
	require.NoError(t, err)
	_, err = kb.CreateMulti("shared", multisig.NewPubKeyMultisigThreshold(1, []crypto.PubKey{pub}))
	require.NoError(t, err)

	_, err = exact.KeybaseSigner(kb, "watch-only", "")
	require.ErrorContains(t, err, "offline key, which cannot sign")
	_, err = exact.KeybaseSigner(kb, "shared", "")
	require.ErrorContains(t, err, "multi key, which cannot sign")
}

// TestKeybaseSignerSignsConcurrentlyFromAKeybaseOnDisk: gnokey's keybase on
// disk locks its database for each signature, so payments signing at once
// take their turns rather than failing all but one.
func TestKeybaseSignerSignsConcurrentlyFromAKeybaseOnDisk(t *testing.T) {
	kb, err := keys.NewKeyBaseFromDir(t.TempDir())
	require.NoError(t, err)
	_, err = kb.CreateAccount("buyer", test1Mnemonic, "", "pw", 0, 0)
	require.NoError(t, err)
	signer, err := exact.KeybaseSigner(kb, "buyer", "pw")
	require.NoError(t, err)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { _, errs[i] = signer.Sign([]byte("sign bytes")) })
	}
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
}
