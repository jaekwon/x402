package exact_test

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gnolang/gno/gno.land/pkg/gnoclient"
	rpcclient "github.com/gnolang/gno/tm2/pkg/bft/rpc/client"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/crypto/keys"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"

	"github.com/gnoverse/x402/client/exact"
)

// A buyer pays the onyx testnet's demo seller from a key gnokey keeps. The
// gno-aware lines are the signer, the node and the Register call; recognising
// the 402, choosing the offer, encoding PAYMENT-SIGNATURE and retrying are
// upstream's own client.
func ExampleNewExactGnoScheme() {
	kb, err := keys.NewKeyBaseFromDir("/home/me/.config/gno") // gnokey's home
	if err != nil {
		panic(err)
	}
	signer, err := exact.KeybaseSigner(kb, "buyer", "the key's password")
	if err != nil {
		panic(err)
	}

	// The node the payment reads its account state and gas price from, on the
	// chain the scheme is registered for.
	rpc, err := rpcclient.NewHTTPClient("https://rpc.onyx.testnets.gno.land:443",
		rpcclient.WithRequestTimeout(10*time.Second))
	if err != nil {
		panic(err)
	}
	node := exact.NewGnoclientNode(&gnoclient.Client{RPCClient: rpc})

	// A stock client pays only in USD-pegged assets until told otherwise, and
	// ugnot is not one. MaxAmountPerPayment is the most one request may cost, in
	// ugnot; the network fee is paid on top.
	client := x402.Newx402Client().
		Register("gno:onyx-1", exact.NewExactGnoScheme(signer, node)).
		SetSpendControls(x402.SpendControls{
			AllowedAssets: []x402.SpendControlAsset{
				{Network: "gno:onyx-1", Asset: "ugnot", MaxAmountPerPayment: "1000000"},
			},
		})
	// The timeout bounds the whole purchase, both requests and the settlement
	// the seller waits for; without one, a seller that never answers holds the
	// buyer forever.
	paying := x402http.WrapHTTPClientWithPayment(&http.Client{Timeout: time.Minute}, x402http.Newx402HTTPClient(client))

	res, err := paying.Get("https://x402-demo.onyx.testnets.gno.land/weather")
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()
	weather, _ := io.ReadAll(res.Body)
	fmt.Println(res.Status, string(weather))
}

// A session key pays from its owner's account: the owner registered the key's
// public key on chain with `gnokey maketx session create`, limited to bank
// sends, with a spend limit and an expiry the chain enforces.
func ExampleWithMaster() {
	kb := keys.NewInMemory()
	if _, err := kb.CreateAccount("session", "<the session key's mnemonic>", "", "", 0, 0); err != nil {
		panic(err)
	}
	signer, err := exact.KeybaseSigner(kb, "session", "")
	if err != nil {
		panic(err)
	}
	owner, err := crypto.AddressFromBech32("g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5")
	if err != nil {
		panic(err)
	}
	rpc, err := rpcclient.NewHTTPClient("https://rpc.onyx.testnets.gno.land:443")
	if err != nil {
		panic(err)
	}

	scheme := exact.NewExactGnoScheme(signer,
		exact.NewGnoclientNode(&gnoclient.Client{RPCClient: rpc}), exact.WithMaster(owner))
	_ = x402.Newx402Client().Register("gno:onyx-1", scheme)
}
