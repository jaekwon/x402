package exact

import (
	"fmt"
	"sync"

	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/crypto/keys"
)

// KeybaseSigner returns the Signer for the key nameOrBech32 names in kb,
// unlocked with password: a key gnokey keeps in its home directory
// (keys.NewKeyBaseFromDir) or one held in memory (keys.NewInMemory), a Ledger
// key included.
//
// The key is resolved once, here, and found again by its address on every
// payment, so renaming it in the keybase later does not change what signs.
// The password is not checked here: a wrong one fails the first payment,
// before anything is signed. A key the keybase holds only the public half of,
// an offline or a multisig key, is refused here: it can never sign, and a
// payment would otherwise read the chain first and fail after.
func KeybaseSigner(kb keys.Keybase, nameOrBech32, password string) (Signer, error) {
	info, err := kb.GetByNameOrAddress(nameOrBech32)
	if err != nil {
		return nil, err
	}
	if t := info.GetType(); t == keys.TypeOffline || t == keys.TypeMulti {
		return nil, fmt.Errorf("key %s is an %s key, which cannot sign", nameOrBech32, t)
	}
	return &keybaseSigner{
		kb:       kb,
		address:  info.GetAddress().String(),
		password: password,
		pub:      info.GetPubKey(),
	}, nil
}

// keybaseSigning makes the signatures this process takes from keybases one at a
// time. gnokey's keybase on disk (keys.NewKeyBaseFromDir) opens its database
// for each operation and holds an exclusive lock on it through the key's
// decryption, so two payments signing at once, or two session keys kept in one
// home, would fail all but one. Another process using the same home at that
// moment, such as gnokey itself, can still fail a signature; nothing is paid
// then, and the payment can be made again.
var keybaseSigning sync.Mutex

// keybaseSigner signs with a key a keys.Keybase holds.
type keybaseSigner struct {
	kb       keys.Keybase
	address  string
	password string
	pub      crypto.PubKey
}

func (s *keybaseSigner) PubKey() crypto.PubKey { return s.pub }

// Sign signs msg with the key, and refuses a signature made by any other: the
// payment names PubKey as its signer, so a key that resolves to another one
// since, such as a Ledger swapped for another device, would sign a payment no
// account verifies.
func (s *keybaseSigner) Sign(msg []byte) ([]byte, error) {
	keybaseSigning.Lock()
	defer keybaseSigning.Unlock()
	sig, pub, err := s.kb.Sign(s.address, s.password, msg)
	if err != nil {
		return nil, err
	}
	if pub == nil || !pub.Equals(s.pub) {
		return nil, fmt.Errorf("key %s signed with a public key other than its own", s.address)
	}
	return sig, nil
}
