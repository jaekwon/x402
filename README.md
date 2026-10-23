# x402 on gno.land

> [!WARNING]
> **Unaudited work in progress: point it at testnets only, never at real funds.** Interfaces, the
> reason vocabulary and the payload shape are still moving, and releases may break each other.

[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Container](https://img.shields.io/badge/ghcr.io-gnoverse%2Fgnofacilitator-2496ED?logo=docker&logoColor=white)](https://github.com/gnoverse/x402/pkgs/container/gnofacilitator)

> Charge for an HTTP endpoint in ugnot, and let any x402 client pay it.

[x402](https://x402.org) is the payment protocol behind HTTP 402: a server answers `402` with the
terms it accepts, a client pays, and the request succeeds. This adds gno.land as one of those terms.
The resource is any endpoint — no realm and no contract call are involved.

The protocol, route matching, header encoding and the retry come from the ecosystem's own
middleware. This supplies the chain-specific parts:

- **Seller mechanism** — list gno in a route's `accepts[]` and the middleware prices it, offers it and reads the payment.
- **`gnofacilitator`** — verifies payments and broadcasts them. It holds no keys.
- **Client mechanism** — register it on a stock x402 client, list ugnot in the client's spend controls, and the client pays a gno chain.

**Try it:** <https://x402-demo.onyx.testnets.gno.land/weather> sells sample weather for 0.25 GNOT
on the onyx testnet (`gno:onyx-1`). A browser with the [Adena](https://adena.app) wallet pays from
the page, and a terminal pays with the two commands in
[examples/weather](examples/weather#paying-it).

## Sell something

```go
routes := x402http.RoutesConfig{
    "GET /weather": {
        Accepts: x402http.PaymentOptions{{
            Scheme:  "exact",
            PayTo:   "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq",
            Price:   x402.AssetAmount{Asset: "ugnot", Amount: "250000"},
            Network: "gno:dev",
        }},
        Description: "Weather data",
    },
}

handler := nethttpmw.X402Payment(nethttpmw.Config{
    Routes:      routes,
    Facilitator: x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: facilitatorURL}),
    Schemes:     []nethttpmw.SchemeConfig{{Network: "gno:dev", Server: gnoexact.NewExactGnoScheme()}},
})(mux)
```

An unpaid request gets `402`, with the terms in the `PAYMENT-REQUIRED` header as base64 JSON — v2
carries them there, not in the body:

```json
{ "x402Version": 2,
  "error": "Payment required",
  "resource": { "url": "http://localhost:8401/weather", "description": "Weather data",
                "mimeType": "application/json" },
  "accepts": [{
    "scheme": "exact", "network": "gno:dev", "asset": "ugnot", "amount": "250000",
    "payTo": "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq",
    "maxTimeoutSeconds": 300,
    "extra": { "areFeesSponsored": false }
  }] }
```

Runnable, against a facilitator and a real chain → **[examples/weather](examples/weather)**.

A public facilitator serves gno.land's testnet at `https://facilitator.onyx.testnets.gno.land`.
Its network is `gno:onyx-1`, so a seller using it sets `Network` to that. You can also
[run your own](docs/deploying.md).

## Pay for it

```sh
npm i @gnoverse/x402-gno
```

```js
import { ExactGnoScheme } from "@gnoverse/x402-gno/exact/client";

const client = new x402Client()
  .register("gno:*", new ExactGnoScheme(wallet))
  .setSpendControls({
    allowedAssets: [{ network: "gno:*", asset: "ugnot", maxAmountPerPayment: "1000000" }],
  });
const paid = wrapFetchWithPayment(fetch, client);
const res = await paid("https://api.example.com/weather");
```

From Go, register [`client/exact`](client/exact) on upstream's Go client:

```go
import clientexact "github.com/gnoverse/x402/client/exact"

signer, _ := clientexact.KeybaseSigner(kb, "buyer", password)           // a key gnokey keeps
node := clientexact.NewGnoclientNode(&gnoclient.Client{RPCClient: rpc}) // a node on onyx-1

client := x402.Newx402Client().
	Register("gno:onyx-1", clientexact.NewExactGnoScheme(signer, node)).
	SetSpendControls(x402.SpendControls{AllowedAssets: []x402.SpendControlAsset{
		{Network: "gno:onyx-1", Asset: "ugnot", MaxAmountPerPayment: "1000000"},
	}})
paid := x402http.WrapHTTPClientWithPayment(&http.Client{Timeout: time.Minute}, x402http.Newx402HTTPClient(client))
res, err := paid.Get("https://api.example.com/weather")
```

A stock client — `@x402/core` from 2.23, and upstream's Go client — pays only in USD-pegged assets
unless an asset is listed in its spend controls, so the buyer lists ugnot. `maxAmountPerPayment` is
the most one request may cost, in ugnot; the network fee is paid on top.

Recognising the 402, selecting an entry, encoding `PAYMENT-SIGNATURE` and retrying are the stock
client's own code, unmodified: `@x402/fetch`, or upstream's `x402http`. The payment is signed against
the chain id the **offer** names rather than the one the wallet's node reports — see
[client/README.md](client/README.md) for what that does and does not buy you.

An agent can pay with a [session key](client/README.md#paying-with-a-session-key) instead of its
owner's key: `new ExactGnoScheme(wallet, { master })` in JavaScript, `clientexact.WithMaster(owner)`
in Go, where the owner's address names the account that pays and the signer holds a key the owner
registered on chain with a spend limit and an expiry. The chain enforces both.

## What's in here

An x402 mechanism has three roles — **server** (the seller), **facilitator**, **client** (the buyer)
— and the ecosystem names them the same way in every language: `@x402/evm` publishes
`./exact/server`, `./exact/facilitator` and `./exact/client`, and its Go module has the matching
directories. The top level here is that grid. Which language a role is written in is an
implementation detail of the role, so no directory names one.

| Role | Path | What |
|------|------|------|
| server | `server/exact/` | upstream's `SchemeNetworkServer`, implemented for gno |
| facilitator | `facilitator/exact/`, `facilitator/` | upstream's `SchemeNetworkFacilitator` for gno, and the `/verify` `/settle` `/supported` service built on upstream's engine |
| client | `client/src/exact/client.ts`, `client/exact/` | upstream's `SchemeNetworkClient`, in TypeScript and in Go |

Plus `cmd/gnofacilitator/` (the binary), `examples/weather/` (a seller and a buyer that make one
payment end to end, with a Makefile to run them) and `e2e/` (real payments through real nodes —
its own Go module).

Both manifests — `go.mod` and `package.json` — live at the root, so `go test ./...`, `npm test` and
the root Makefile's targets run from here. The demo's Makefile runs from `examples/weather`.

## Payment model

- **The payer pays the network fee.** `std.Fee` names no payer and the chain charges the first signer, so the payer of the resource is also the payer of the gas. Offers state this as `extra.areFeesSponsored: false`.
- **The facilitator holds no key.** It verifies offline and broadcasts a transaction the payer already signed, so it cannot move funds anywhere the payer did not sign for.
- **A failed resource is not charged for.** The flow is `authorization`: verify, serve, then settle. The middleware buffers the response and skips settlement on any status ≥400. Skipping it does not void the payment, which stays signed and valid (see below).
- **A failed settlement withholds the response.** The buffered body is discarded.
- **A payment no block executed in time is pending, not failed.** The facilitator answers
  `settlement_pending` with the transaction hash. The seller's SDK settles once more, which asks
  the chain about that hash rather than broadcasting again, and a buyer still not served gets the
  hash to look up.
- **A payment settles once per facilitator.** A copy presented while it settles, or within five
  minutes after, is refused as `duplicate_settlement`. The record is in memory, so run one
  facilitator process per chain.
- **Prices name their denomination.** gno has no default asset, so `"$0.001"` errors rather than resolving to a token.

**Accepted risk:** between `/verify` and `/settle` a payer can consume the signed-over account
sequence with another transaction. gno has no pull-settlement primitive, so this is cooperative — the
same exposure XRPL documents for its own `exact` mechanism.

**A signed payment does not expire.** A gno transaction carries no expiry, and `maxTimeoutSeconds`
binds nothing in it. Until the payer spends its sequence on another transaction, whoever holds the
`PAYMENT-SIGNATURE` header, the seller included, can still broadcast it. A buyer refused service
voids an unused payment by sending any other transaction from the paying account.

## Development

```bash
make test                # Library tests (no chain, no npm)
make js-test             # Client mechanism, including the sign-doc byte-equality check
make test-e2e            # Real payments against in-memory nodes
make lint                # go vet (both modules) + gofmt -l
make build               # bin/gnofacilitator
make help                # Every target
```

`make test-e2e` starts a node via gno.land's txtar harness, stands up the facilitator and a priced
endpoint, pays with the JS client and with the Go client, and asserts the balances changed. It also settles one
payment twice at once in three arrangements: two facilitators sharing a node, two facilitators on
two peered nodes, and one facilitator balancing over both. It asserts how many report the payment
settled, and replays a settled payment to assert it is refused.

Built against [`github.com/x402-foundation/x402/go/v2`](https://github.com/x402-foundation/x402) for
the protocol and [`gnolang/gno`](https://github.com/gnolang/gno) for the chain.

## Conformance

Implemented: the `exact` scheme, the `authorization` flow, v2 headers, `gno:<chain-id>` as a CAIP-2
network, prices in native units.

Not implemented: dollar-string pricing, a browser paywall, payload `extensions`, and a CAIP-2
namespace registration — the last is not required by the schema (`near:testnet` ships without one).

## License

Apache-2.0. See [LICENSE](LICENSE).
