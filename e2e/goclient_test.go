package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gnolang/gno/gno.land/pkg/gnoclient"
	"github.com/gnolang/gno/gno.land/pkg/integration"
	rpcclient "github.com/gnolang/gno/tm2/pkg/bft/rpc/client"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/crypto/keys"
	"github.com/rogpeppe/go-internal/testscript"
	"github.com/stretchr/testify/require"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"

	clientexact "github.com/gnoverse/x402/client/exact"
)

// TestAGoClientPaysAGnoSeller is the Go buyer's half of the claim: upstream's
// Go client, taught gno by registering client/exact, pays a gno seller through
// the facilitator, and the chain agrees.
//
// Its scenarios live in their own directory and need no JS toolchain, so a
// Go-only checkout runs them even where the JS buyer's scenarios skip.
func TestAGoClientPaysAGnoSeller(t *testing.T) {
	params := testscript.Params{
		Dir: "testdata/goclient",
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"x402facilitator": facilitatorCmd,
			"x402seller":      sellerCmd,
			"x402gobuyer":     goBuyerCmd,
		},
	}

	writableGnoRoot(t)
	require.NoError(t, integration.SetupGnolandTestscript(t, &params))
	testscript.Run(t, params)
}

// goBuyerReport is what x402gobuyer prints for a purchase that reached the
// seller: the fields buyer.mjs reports, under the same names, so a scenario
// greps either buyer the same way.
type goBuyerReport struct {
	Status      int    `json:"status"`
	Body        string `json:"body"`
	Payer       string `json:"payer"`
	Transaction string `json:"transaction"`
	Reason      string `json:"reason"`
}

// goBuyerCmd buys from the seller x402seller stood up, in this process: one
// request through an http.Client wrapped by upstream's Go client, with the gno
// scheme registered and ugnot admitted by the spend controls. Recognising the
// 402, choosing the offer, encoding PAYMENT-SIGNATURE and retrying are
// upstream's own code; the gno-aware lines are the signer, the node and the
// register call.
//
// It signs with X402_MNEMONIC, test1's when unset, and with X402_SESSION_MASTER
// set pays as a session of that account. A purchase that reached the seller is
// reported as JSON on stdout; one that never did, because no payment could be
// signed, prints "not paid:" and why. Either way the command succeeds only for a
// purchase served with 200, so a scenario expecting a refusal runs
// `! x402gobuyer`.
func goBuyerCmd(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 0 {
		ts.Fatalf("usage: x402gobuyer")
	}
	seller := ts.Getenv("X402_SELLER_URL")
	if seller == "" {
		ts.Fatalf("x402gobuyer: no seller is running; call x402seller first")
	}
	mnemonic := ts.Getenv("X402_MNEMONIC")
	if mnemonic == "" {
		mnemonic = integration.DefaultAccount_Seed
	}

	kb := keys.NewInMemory()
	_, err := kb.CreateAccount("buyer", mnemonic, "", "", 0, 0)
	ts.Check(err)
	signer, err := clientexact.KeybaseSigner(kb, "buyer", "")
	ts.Check(err)
	rpc, err := rpcclient.NewHTTPClient(httpURL(ts.Getenv("X402_GNO_RPC")))
	ts.Check(err)
	var opts []clientexact.Option
	if master := ts.Getenv("X402_SESSION_MASTER"); master != "" {
		owner, err := crypto.AddressFromBech32(master)
		ts.Check(err)
		opts = append(opts, clientexact.WithMaster(owner))
	}
	node := clientexact.NewGnoclientNode(&gnoclient.Client{RPCClient: rpc})

	// No cap: refused scenarios price a payment past the payer's balance so that
	// the chain refuses it, which a cap would pre-empt.
	client := x402.Newx402Client().
		Register(network, clientexact.NewExactGnoScheme(signer, node, opts...)).
		SetSpendControls(x402.SpendControls{
			AllowedAssets: []x402.SpendControlAsset{{Network: network, Asset: "ugnot"}},
		})
	httpClient := x402http.Newx402HTTPClient(client)

	method := ts.Getenv("X402_METHOD")
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequest(method, seller, nil)
	ts.Check(err)
	// Bounded, because testscript's deadline reaches only the processes it
	// execs: a seller that never answered an in-process buyer would otherwise
	// hold the whole test binary until go test's own timeout.
	res, err := x402http.WrapHTTPClientWithPayment(&http.Client{Timeout: time.Minute}, httpClient).Do(req)
	if err != nil {
		_, _ = io.WriteString(ts.Stdout(), "not paid: "+err.Error()+"\n")
		if !neg {
			ts.Fatalf("x402gobuyer: %v", err)
		}
		return
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	ts.Check(err)

	headers := map[string]string{}
	for name := range res.Header {
		headers[name] = res.Header.Get(name)
	}
	report := goBuyerReport{Status: res.StatusCode, Body: string(body)}
	if settled, err := httpClient.GetPaymentSettleResponse(headers); err == nil {
		report.Payer = settled.Payer
		report.Transaction = settled.Transaction
		report.Reason = strings.TrimSpace(settled.ErrorReason)
	}
	// A verify refusal carries its reason in the challenge, a settle failure in
	// the receipt.
	if report.Reason == "" && res.StatusCode == http.StatusPaymentRequired {
		if challenge, err := httpClient.GetPaymentRequiredResponse(headers, body); err == nil {
			report.Reason = strings.TrimSpace(challenge.Error)
		}
	}
	encoded, err := json.Marshal(report)
	ts.Check(err)
	_, _ = ts.Stdout().Write(encoded)

	if paid := res.StatusCode == http.StatusOK; paid == neg {
		ts.Fatalf("x402gobuyer: status %d, reason %q", res.StatusCode, report.Reason)
	}
}
