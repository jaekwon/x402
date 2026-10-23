// Package exact is the client half of gno's "exact" x402 mechanism: an
// x402.SchemeNetworkClient that signs a payment for a gno chain. Register it on
// upstream's client and that client pays gno sellers:
//
//	client := x402.Newx402Client().
//		Register("gno:onyx-1", exact.NewExactGnoScheme(signer, node)).
//		SetSpendControls(x402.SpendControls{AllowedAssets: []x402.SpendControlAsset{
//			{Network: "gno:onyx-1", Asset: "ugnot", MaxAmountPerPayment: "1000000"},
//		}})
//
// It does nothing else: no HTTP, no 402 handling, no broadcasting. The client
// library owns the protocol, the facilitator owns settlement, and this owns only
// the chain-specific step between them. Its payload is the one the TypeScript
// mechanism in client/src produces: base64 of a fully signed, unbroadcast std.Tx
// carrying a single bank.MsgSend.
//
// The chain id a payment is signed for comes from the offer, which tm2 makes
// direct: std.Tx.GetSignBytes takes the chain id as an argument rather than
// asking a node for it.
package exact

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"strings"

	"github.com/gnolang/gno/gno.land/pkg/gnoland/ugnot"
	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/crypto/multisig"
	"github.com/gnolang/gno/tm2/pkg/sdk/bank"
	"github.com/gnolang/gno/tm2/pkg/std"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"

	facilitatorexact "github.com/gnoverse/x402/facilitator/exact"
)

const schemeExact = "exact"

// namespace is the CAIP-2 namespace every gno network shares: a network is
// "gno:<chain-id>".
const namespace = "gno"

// A gno buyer pays its own transaction fee: std.Fee names no payer and the ante
// charges the first signer, so the payer of the payment is also the payer of the
// fee. The requirements carry no gas fields, so the buyer chooses.
//
// The chain takes the whole fee offered, not the gas used, so the fee follows
// the chain's gas price for the gas the payment reserves. That price adapts to
// load, and a payment under it is refused at settle after the resource ran,
// costing the buyer the resource; it is offered gasPriceMargin times over. A
// block above the chain's gas target raises the price's amount by at least 1, so
// at onyx-1's 1ugnot per 1000 gas the margin covers exactly one such block
// between signing and settle. A bank send uses about an eighth of gasWanted.
//
// Both numbers are the TypeScript mechanism's, so the two sign the same payment
// for the same chain state.
const (
	gasWanted      = 10_000_000
	gasPriceMargin = 2
)

// unpricedGasFee is the fee offered when the chain reports no gas price: a node
// may still set a minimum of its own, which the chain does not report, so this
// is well clear of any.
var unpricedGasFee = std.Coin{Denom: ugnot.Denom, Amount: 1_000_000}

// maxGasFee is the most a payment offers as its network fee: 1 GNOT, the fee
// offered to a chain that reports no gas price. The fee follows the gas price
// the buyer's node reports, and a node can report any price, while neither
// the spend cap nor the facilitator bounds the fee and the chain keeps all of
// it. At onyx-1's price a payment's fee is 0.02 GNOT.
var maxGasFee = std.Coin{Denom: ugnot.Denom, Amount: 1_000_000}

// denomination is tm2's denomination grammar (std.ValidateDenom), capped at its
// MaxDenomLength of 274 bytes. The coin a payment sends is amount and asset as
// one string, so an asset outside it is either refused by the chain or read as
// another coin: "0ugnot" after "250000" is 2500000ugnot.
var denomination = regexp.MustCompile(`^[a-z/][a-z0-9_.:/-]{2,273}$`)

// positiveAmount is a whole number above zero, written without a sign or a
// leading zero.
var positiveAmount = regexp.MustCompile(`^[1-9][0-9]*$`)

// atomicAmount is a spend cap as upstream's client resolves one: a whole number
// of the asset's smallest unit.
var atomicAmount = regexp.MustCompile(`^[0-9]+$`)

// ErrInvalidRequirements reports requirements this mechanism will not sign a
// payment for: an offer for no gno chain or for another scheme, a malformed
// price or payee, or terms the mechanism does not implement.
//
// The requirements arrive from a seller over the network, so they are checked
// rather than trusted: a malformed amount or a missing payTo would otherwise
// become a signed transaction that pays the wrong thing.
var ErrInvalidRequirements = errors.New("x402-gno: payment requirements refused")

// ErrExceedsSpendCap reports a price above the per-payment cap the client's
// spend controls set for its asset. The cap bounds the price; the fee is paid on
// top of it.
var ErrExceedsSpendCap = errors.New("x402-gno: price exceeds the spend cap")

// ErrExcessiveFee reports a network fee above the most a payment offers, 1
// GNOT: the gas price the node reported is either wrong or not one to pay at.
var ErrExcessiveFee = errors.New("x402-gno: network fee too high")

// Signer holds the key a payment is signed with: the paying account's own key,
// or a session key the paying account registered (see WithMaster).
//
// A crypto.PrivKey is one. KeybaseSigner adapts a key gnokey keeps, a Ledger
// key included.
type Signer interface {
	// PubKey is the key the chain verifies the signature against.
	PubKey() crypto.PubKey

	// Sign returns the signature of msg, the payment's sign bytes.
	Sign(msg []byte) ([]byte, error)
}

// Node is the chain access a payment needs before it is signed: which chain the
// node serves, the number and sequence of the account the signature is verified
// against, and the gas price the fee follows. Nothing is broadcast through it;
// the seller's facilitator broadcasts the payment. Implemented by the gnoclient
// adapter NewGnoclientNode and by fakes in tests.
type Node interface {
	// NodeChainID is the chain id the node reports for itself.
	NodeChainID(ctx context.Context) (string, error)

	// Account is the on-chain account of addr. An address the chain holds no
	// account for is an error: it has nothing to pay with.
	Account(ctx context.Context, addr crypto.Address) (std.Account, error)

	// SessionAccount is the session account session holds under master.
	SessionAccount(ctx context.Context, master, session crypto.Address) (std.Account, error)

	// GasPrice is the gas price the chain reports, or nil when it reports none.
	GasPrice(ctx context.Context) (*std.GasPrice, error)
}

// ExactGnoScheme signs gno payments for the "exact" scheme. Register it on an
// x402 client for the gno network its node serves.
//
// It keeps nothing between payments and is safe for concurrent use. Payments
// from one account still share that account's sequence: of two signed over the
// same sequence, at most one settles. A session has a sequence of its own, so
// give each concurrent stream of payments its own session.
type ExactGnoScheme struct {
	signer Signer
	node   Node
	master crypto.Address // zero when the signer's own account pays
}

var _ x402.SchemeNetworkClient = (*ExactGnoScheme)(nil)

// Option configures an ExactGnoScheme.
type Option func(*ExactGnoScheme)

// WithMaster pays from master's account with a session key: the signer holds a
// key master registered on chain as a session, not master's own key. Payments
// move master's coins within the spend limit, expiry and allowed message types
// the chain holds for the session, and the settlement names master as the payer.
// A session limited to bank/send can pay any x402 seller and do nothing else.
func WithMaster(master crypto.Address) Option {
	return func(s *ExactGnoScheme) { s.master = master }
}

// NewExactGnoScheme returns the mechanism that signs with signer and reads the
// chain through node.
//
// Register it for the network node serves. A payment signs over the account
// number and sequence node reports, and those exist only on node's chain, so a
// payment for any other chain is refused before anything is signed. To pay on
// more than one gno chain, register one scheme per network, each with a node on
// that network.
func NewExactGnoScheme(signer Signer, node Node, opts ...Option) *ExactGnoScheme {
	s := &ExactGnoScheme{signer: signer, node: node}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Scheme names the x402 payment scheme this implements.
func (s *ExactGnoScheme) Scheme() string { return schemeExact }

// CreatePaymentPayload signs the payment requirements ask for and returns the
// scheme payload, one field: transaction, base64 of a fully signed, unbroadcast
// std.Tx carrying a single bank.MsgSend of exactly the price to payTo, its memo
// bound to extra.memo when the requirements name one. Upstream's client adds
// the accepted requirements, the resource and the extensions around it.
//
// The requirements are checked before the chain is read, and the chain before
// anything is signed. payloadCtx.MaxAmountPerPayment, the cap the client's
// spend controls resolved for the asset, bounds the price when it is set.
func (s *ExactGnoScheme) CreatePaymentPayload(ctx context.Context, requirements types.PaymentRequirements, payloadCtx x402.PaymentPayloadContext) (types.PaymentPayload, error) {
	if s.signer == nil || s.node == nil {
		return types.PaymentPayload{}, errors.New("x402-gno: the scheme needs both a signer and a node")
	}
	payment, err := Prepare(ctx, s.node, s.signer.PubKey(), s.master, requirements, payloadCtx.MaxAmountPerPayment)
	if err != nil {
		return types.PaymentPayload{}, err
	}
	// The reads can outlast the caller. A payment nobody waits for any more is
	// not signed: a signature is the one step that cannot be taken back, and a
	// device would ask its owner to approve a purchase already abandoned.
	if err := ctx.Err(); err != nil {
		return types.PaymentPayload{}, fmt.Errorf("x402-gno: the payment was given up before it was signed: %w", err)
	}
	signBytes, err := payment.SignBytes()
	if err != nil {
		return types.PaymentPayload{}, err
	}
	signature, err := s.signer.Sign(signBytes)
	if err != nil {
		return types.PaymentPayload{}, fmt.Errorf("x402-gno: sign the payment: %w", err)
	}
	return payment.Payload(signature)
}

// Payment is a payment built for a seller's requirements and not yet signed:
// the transaction, and what a signature over it commits to. Prepare makes one.
type Payment struct {
	// Tx is the payment without its signature: one bank.MsgSend of exactly the
	// price to payTo, the fee, and the memo the requirements bind.
	Tx std.Tx

	// ChainID is the chain the offer names. AccountNumber and Sequence are the
	// number and sequence of the account the chain verifies the signature
	// against: the payer's, or the session's when a session key signs.
	ChainID       string
	AccountNumber uint64
	Sequence      uint64

	// PubKey is the key that must sign.
	PubKey crypto.PubKey

	// SessionAddr is the session account that signs for the payer, or zero
	// when the payer's own key signs.
	SessionAddr crypto.Address
}

// SignBytes is what a signature over the payment covers.
func (p *Payment) SignBytes() ([]byte, error) {
	signBytes, err := p.Tx.GetSignBytes(p.ChainID, p.AccountNumber, p.Sequence)
	if err != nil {
		return nil, fmt.Errorf("x402-gno: build the sign doc: %w", err)
	}
	return signBytes, nil
}

// Payload attaches signature, PubKey's signature over the payment, and returns
// the scheme payload CreatePaymentPayload would: base64 of the signed std.Tx.
//
// The signature is verified first, the way the chain verifies it, so a key
// that signed anything else — another chain, other account state, another
// transaction — is refused here rather than by the seller's facilitator. Both
// renderings of the sign doc the chain accepts are accepted, so a signer that
// renders the fee as gas_wanted/gas_fee, as older gnokey and gno-js-client do,
// can pay too.
func (p *Payment) Payload(signature []byte) (types.PaymentPayload, error) {
	if p.PubKey == nil {
		return types.PaymentPayload{}, errors.New("x402-gno: the payment names no key to verify its signature with")
	}
	// The chain verifies the payer's own signature against the account
	// MsgSend.FromAddress names, and a session's against the session account,
	// so PubKey has to be that account's key.
	signer := p.SessionAddr
	if signer.IsZero() && len(p.Tx.Msgs) == 1 {
		if send, ok := p.Tx.Msgs[0].(bank.MsgSend); ok {
			signer = send.FromAddress
		}
	}
	if p.PubKey.Address() != signer {
		return types.PaymentPayload{}, fmt.Errorf("x402-gno: key %s is not the key of %s, the account the payment is signed for", p.PubKey.Address(), signer)
	}
	rendering, err := std.VerifySignaturePayload(p.PubKey, p.Tx.SignDoc(p.ChainID, p.AccountNumber, p.Sequence), signature)
	if err != nil {
		return types.PaymentPayload{}, fmt.Errorf("x402-gno: build the sign doc: %w", err)
	}
	if rendering == std.PayloadRenderingNone {
		return types.PaymentPayload{}, fmt.Errorf("x402-gno: the signature does not verify for key %s over this payment on chain %s", p.PubKey.Address(), p.ChainID)
	}
	tx := p.Tx
	// The chain reads a session's key and limits at session_addr under the
	// master MsgSend.FromAddress names.
	tx.Signatures = []std.Signature{{PubKey: p.PubKey, Signature: signature, SessionAddr: p.SessionAddr}}
	if err := tx.ValidateBasic(); err != nil {
		return types.PaymentPayload{}, fmt.Errorf("x402-gno: the signed payment is not a valid transaction: %w", err)
	}
	raw, err := amino.Marshal(tx)
	if err != nil {
		return types.PaymentPayload{}, fmt.Errorf("x402-gno: encode the payment: %w", err)
	}
	return types.PaymentPayload{
		X402Version: 2,
		Payload:     map[string]any{"transaction": base64.StdEncoding.EncodeToString(raw)},
	}, nil
}

// terms is the payment requirements ask for.
type terms struct {
	chainID string
	payTo   crypto.Address
	price   std.Coin
	memo    string
}

// readTerms checks requirements and returns the payment they ask for. Every
// refusal wraps ErrInvalidRequirements.
func readTerms(req types.PaymentRequirements) (terms, error) {
	chainID, ok := chainIDOf(req.Network)
	if !ok {
		return terms{}, refuse("%s does not name a gno chain", req.Network)
	}
	if req.Scheme != schemeExact {
		return terms{}, refuse("scheme %s is not %s", req.Scheme, schemeExact)
	}
	if !denomination.MatchString(req.Asset) {
		return terms{}, refuse("asset %s is not a gno denomination", req.Asset)
	}
	if !positiveAmount.MatchString(req.Amount) {
		return terms{}, refuse("amount %s is not a positive integer", req.Amount)
	}
	if req.PayTo == "" {
		return terms{}, refuse("requirements name no payTo")
	}
	// The facilitator compares the recipient's canonical form with payTo as
	// written, so a payTo that decodes but is written otherwise, such as in
	// upper case, names an address no payment to it can match.
	payTo, err := crypto.AddressFromBech32(req.PayTo)
	if err != nil || payTo.String() != req.PayTo || payTo.IsZero() {
		return terms{}, refuse("payTo %s is not a gno address", req.PayTo)
	}
	// §6.1 reserves both keys: an offer naming a transfer method or a flow this
	// mechanism does not implement is not one it may sign a payment for. A key
	// set to null names nothing, as upstream's SDKs read it.
	if v := req.Extra["assetTransferMethod"]; v != nil && v != facilitatorexact.AssetTransferMethod {
		return terms{}, refuse("assetTransferMethod %v is not %s", v, facilitatorexact.AssetTransferMethod)
	}
	if v := req.Extra["paymentFlow"]; v != nil && v != string(x402.PaymentFlowAuthorization) {
		return terms{}, refuse("paymentFlow %v is not %s", v, x402.PaymentFlowAuthorization)
	}
	memo, err := boundMemo(req.Extra)
	if err != nil {
		return terms{}, err
	}
	// The chain's own coin parse is the one the facilitator matches the payment
	// against, and it refuses what the grammar above admits but no coin holds,
	// such as an amount past the range of one.
	coins, err := std.ParseCoins(req.Amount + req.Asset)
	if err != nil || len(coins) != 1 {
		return terms{}, refuse("%s%s is not one coin the chain can hold", req.Amount, req.Asset)
	}
	return terms{chainID: chainID, payTo: payTo, price: coins[0], memo: memo}, nil
}

// refuse is a refusal of the requirements, wrapping ErrInvalidRequirements.
func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidRequirements}, args...)...)
}

// chainIDOf returns the gno chain id a CAIP-2 network string names, and false
// when the string names no gno chain.
//
// The chain id has to be one CAIP-2 can name, the grammar the facilitator holds
// its own chain id to: tm2 accepts others, but no facilitator can serve them,
// so a payment for one could never settle. That also refuses a wildcard such as
// "gno:*", which names a pattern rather than a chain.
func chainIDOf(network string) (string, bool) {
	ns, chainID, _ := strings.Cut(network, ":")
	if ns != namespace || facilitatorexact.ValidChainID(chainID) != nil {
		return "", false
	}
	return chainID, true
}

// boundMemo reads extra.memo, the memo the requirements bind the payment's
// transaction to, and "" when they bind none.
//
// A memo that is present but not a string is a malformed offer rather than an
// absent one, because signing "" against it would produce a payment the seller
// refuses for a reason the buyer cannot see. The cap is the facilitator's: a
// payment signed against a longer memo could never be accepted.
func boundMemo(extra map[string]any) (string, error) {
	v, ok := extra["memo"]
	if !ok {
		return "", nil
	}
	memo, ok := v.(string)
	if !ok {
		return "", refuse("extra.memo is %T, want string", v)
	}
	if len(memo) > facilitatorexact.MaxMemoBytes {
		return "", refuse("extra.memo is %d bytes, exceeding the %d-byte maximum", len(memo), facilitatorexact.MaxMemoBytes)
	}
	return memo, nil
}

// withinCap refuses a price above limit, the per-payment cap the client's spend
// controls resolved for the price's asset; "" caps nothing.
//
// Upstream's client drops an offer above the cap before it picks one, so a
// price over it reaches here only when a payment is asked for directly rather
// than chosen from a 402. It is refused all the same: this is the last point
// before a signature exists.
func withinCap(price std.Coin, limit string) error {
	if limit == "" {
		return nil
	}
	if !atomicAmount.MatchString(limit) {
		return fmt.Errorf("x402-gno: spend cap %q is not a whole number of %s", limit, price.Denom)
	}
	most, _ := new(big.Int).SetString(limit, 10)
	if big.NewInt(price.Amount).Cmp(most) > 0 {
		return fmt.Errorf("%w: %s is more than the %s%s one payment may cost", ErrExceedsSpendCap, price, limit, price.Denom)
	}
	return nil
}

// Prepare builds the payment requirements ask for and checks it against the
// chain through node, without signing it. pub is the key that will sign: the
// payer's own, or, with master non-zero, a session key master registered, and
// then master pays. maxAmountPerPayment caps the price in its asset's smallest
// unit; "" caps nothing.
//
// It is CreatePaymentPayload's first half, for a key held where this process
// cannot sign with it, such as by gnokey or on a hardware wallet, or for a
// buyer who wants a person to approve each price: show what Payment.Tx pays,
// have the key sign it, and hand the signature to Payload.
//
// The requirements are checked before the chain is read.
func Prepare(ctx context.Context, node Node, pub crypto.PubKey, master crypto.Address, requirements types.PaymentRequirements, maxAmountPerPayment string) (*Payment, error) {
	t, err := readTerms(requirements)
	if err != nil {
		return nil, err
	}
	if err := withinCap(t.price, maxAmountPerPayment); err != nil {
		return nil, err
	}
	if node == nil {
		return nil, errors.New("x402-gno: a payment needs a node to read the chain from")
	}
	if pub == nil {
		return nil, errors.New("x402-gno: the signer has no public key")
	}
	// The facilitator takes one signature from one signer and refuses a
	// threshold key outright, so a payment signed with one never settles.
	if _, ok := pub.(multisig.PubKeyMultisigThreshold); ok {
		return nil, errors.New("x402-gno: a multisig key cannot pay: the facilitator takes one signature from one signer")
	}
	// The address of the signing key: the payer itself, or the session that
	// signs for the master.
	key := pub.Address()

	// The signature commits to the chain id the offer names and to the account
	// number and sequence this node reports. Both have to describe one chain: a
	// payment signed for one chain over another's account state is refused by
	// both, unless the two accounts happen to agree, and then it pays on a chain
	// whose state this buyer never read.
	served, err := node.NodeChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("x402-gno: ask the node which chain it serves: %w", err)
	}
	if served != t.chainID {
		return nil, fmt.Errorf("x402-gno: the offer is for chain %s and the node serves %s", t.chainID, served)
	}

	price, err := node.GasPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("x402-gno: read the chain's gas price: %w", err)
	}
	fee, err := gasFee(price)
	if err != nil {
		return nil, err
	}
	if fee.Denom != maxGasFee.Denom || fee.Amount > maxGasFee.Amount {
		return nil, fmt.Errorf("%w: the gas price the node reports, %s per %d gas, makes the fee %s, more than the %s a payment offers", ErrExcessiveFee, price.Price, price.Gas, fee, maxGasFee)
	}

	payer, session := key, crypto.Address{}
	if !master.IsZero() {
		payer, session = master, key
	}
	send := bank.MsgSend{FromAddress: payer, ToAddress: t.payTo, Amount: std.Coins{t.price}}
	if err := send.ValidateBasic(); err != nil {
		return nil, fmt.Errorf("x402-gno: the payment is not a valid send: %w", err)
	}

	account, err := readAccount(ctx, node, master, key)
	if err != nil {
		return nil, err
	}
	return &Payment{
		Tx: std.Tx{
			Msgs: []std.Msg{send},
			Fee:  std.NewFee(gasWanted, fee),
			Memo: t.memo,
		},
		ChainID:       t.chainID,
		AccountNumber: account.GetAccountNumber(),
		Sequence:      account.GetSequence(),
		PubKey:        pub,
		SessionAddr:   session,
	}, nil
}

// readAccount returns the account the chain verifies key's signature against:
// the session account key holds under master when master is set, key's own
// account otherwise. Each keeps its own number and sequence.
func readAccount(ctx context.Context, node Node, master, key crypto.Address) (std.Account, error) {
	var (
		acc std.Account
		err error
	)
	if master.IsZero() {
		acc, err = node.Account(ctx, key)
		if err != nil {
			if _, unknown := errors.AsType[std.UnknownAddressError](err); unknown {
				return nil, fmt.Errorf("x402-gno: the chain holds no account for %s; fund it before paying from it: %w", key, err)
			}
			return nil, fmt.Errorf("x402-gno: read the account of %s: %w", key, err)
		}
	} else {
		acc, err = node.SessionAccount(ctx, master, key)
		if err != nil {
			return nil, fmt.Errorf("x402-gno: read session %s of %s: %w", key, master, err)
		}
	}
	// Node is an interface others implement, and a nil account behind a
	// non-nil interface would otherwise panic inside a buyer's request.
	if isNil(acc) {
		return nil, fmt.Errorf("x402-gno: the node answered with no account for %s", key)
	}
	return acc, nil
}

// isNil reports whether v is nil, or an interface holding a nil pointer, as an
// RPC client's constructor returns alongside its error.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// gasFee is the fee a payment offers for gasWanted at price: gasPriceMargin
// times the price, rounded up to a whole unit, or unpricedGasFee when the chain
// reports none.
func gasFee(price *std.GasPrice) (std.Coin, error) {
	if price == nil || price.Gas == 0 || price.Price.Amount == 0 {
		return unpricedGasFee, nil
	}
	if price.Gas < 0 || price.Price.Amount < 0 {
		return std.Coin{}, fmt.Errorf("x402-gno: the chain reports a gas price of %s per %d gas", price.Price, price.Gas)
	}
	gas := big.NewInt(price.Gas)
	fee := new(big.Int).Mul(big.NewInt(gasWanted*gasPriceMargin), big.NewInt(price.Price.Amount))
	fee.Add(fee, gas).Sub(fee, big.NewInt(1)).Quo(fee, gas)
	if !fee.IsInt64() {
		return std.Coin{}, fmt.Errorf("x402-gno: at %s per %d gas the fee exceeds what one coin holds", price.Price, price.Gas)
	}
	coin, err := std.NewCoinSafe(price.Price.Denom, fee.Int64())
	if err != nil {
		return std.Coin{}, fmt.Errorf("x402-gno: the chain's gas price names no coin a fee can be paid in: %w", err)
	}
	return coin, nil
}
