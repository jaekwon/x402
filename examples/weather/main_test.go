package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"

	"github.com/gnoverse/x402/facilitator"
	"github.com/gnoverse/x402/facilitator/exact"
	"github.com/gnoverse/x402/facilitator/exact/exacttest"
)

// unreachableChain stands in for the node. A 402 is decided from the seller's
// own configuration, so a call here means an unpaid request reached the chain.
type unreachableChain struct{}

func (unreachableChain) SignerAccount(context.Context, *std.Tx) (exact.SignerAccount, error) {
	panic("an unpaid request must be answered without reading an account")
}

func (unreachableChain) Simulate(*std.Tx) error {
	panic("an unpaid request must be answered without simulating")
}

func (unreachableChain) Broadcast([]byte) (string, error) {
	panic("an unpaid request must be answered without broadcasting")
}

func (unreachableChain) AwaitDelivery(context.Context, string) error {
	panic("an unpaid request must be answered without waiting for a delivery")
}

// sellerLog is what the seller logs, as JSON lines. The seller writes from its
// own goroutines, so writes and reads share a lock.
type sellerLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *sellerLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// recordsWith returns the logged records that carry key.
func (l *sellerLog) recordsWith(t *testing.T, key string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []map[string]any
	for line := range bytes.Lines(l.buf.Bytes()) {
		var record map[string]any
		require.NoError(t, json.Unmarshal(line, &record))
		if _, ok := record[key]; ok {
			found = append(found, record)
		}
	}
	return found
}

// logged reports whether the seller logged a record with message msg. It fails
// no test, so a condition polled from another goroutine can call it.
func (l *sellerLog) logged(msg string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for line := range bytes.Lines(l.buf.Bytes()) {
		var record struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal(line, &record) == nil && record.Msg == msg {
			return true
		}
	}
	return false
}

// weatherSeller serves the weather seller for exacttest's offer through the
// facilitator at facilitatorURL, after adjust edits its configuration, and
// returns its URL and what it logs.
func weatherSeller(t *testing.T, facilitatorURL string, adjust ...func(*sellerConfig)) (string, *sellerLog) {
	t.Helper()
	log := &sellerLog{}
	cfg := sellerConfig{
		facilitatorURL: facilitatorURL,
		network:        "gno:" + exacttest.ChainID,
		payTo:          exacttest.PayTo,
		price:          std.MustParseCoin("250000ugnot"),
		log:            slog.New(slog.NewJSONHandler(log, nil)),
	}
	for _, edit := range adjust {
		edit(&cfg)
	}
	handler, err := newSeller(t.Context(), cfg)
	require.NoError(t, err)
	seller := httptest.NewServer(handler)
	t.Cleanup(seller.Close)
	return seller.URL, log
}

// termsOf requests the weather without paying and returns the 402's terms.
func termsOf(t *testing.T, sellerURL string) x402.PaymentRequired {
	t.Helper()

	unpaid, err := http.Get(sellerURL + "/weather")
	require.NoError(t, err)
	require.NoError(t, unpaid.Body.Close())
	require.Equal(t, http.StatusPaymentRequired, unpaid.StatusCode)
	terms, err := base64.StdEncoding.DecodeString(unpaid.Header.Get("PAYMENT-REQUIRED"))
	require.NoError(t, err)
	var required x402.PaymentRequired
	require.NoError(t, json.Unmarshal(terms, &required))
	return required
}

// payWith answers the seller's offer with txB64 as the transaction, and returns
// what the seller answered the paid request with.
func payWith(t *testing.T, sellerURL, txB64 string) *http.Response {
	t.Helper()

	required := termsOf(t, sellerURL)
	require.Len(t, required.Accepts, 1)

	payload, err := json.Marshal(types.PaymentPayload{
		X402Version: 2,
		Accepted:    required.Accepts[0],
		Payload:     map[string]any{"transaction": txB64},
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, sellerURL+"/weather", nil)
	require.NoError(t, err)
	req.Header.Set("PAYMENT-SIGNATURE", base64.StdEncoding.EncodeToString(payload))

	paid, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = paid.Body.Close() })
	return paid
}

// browserPage opens the priced route the way a browser does and returns the
// response and the page it carries.
func browserPage(t *testing.T, sellerURL string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, sellerURL+"/weather", nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(body)
}

// TestABrowserIsToldWhatTheResourceCostsInGno opens the priced route the way a
// browser does. The middleware answers a browser with an HTML paywall, and its
// built-in one is a wallet app for EVM and Solana chains that renders nothing
// for a gno network.
func TestABrowserIsToldWhatTheResourceCostsInGno(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	resp, page := browserPage(t, sellerURL)

	require.Equal(t, http.StatusPaymentRequired, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	assert.NotEmpty(t, resp.Header.Get("PAYMENT-REQUIRED"), "the terms still travel in the header")

	assert.NotContains(t, page, "window.x402", "the built-in EVM/Solana paywall was served")
	assert.Contains(t, page, ">0.25<", "the amount a person reads")
	assert.Contains(t, page, "GNOT")
	assert.Contains(t, page, "250000ugnot")
	assert.Contains(t, page, "gno:dev")
	assert.Contains(t, page, "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq")
	assert.Contains(t, page, "buy.mjs", "the page names a buyer that can pay it without a wallet")
	assert.Contains(t, page, "Pay 0.25 GNOT with Adena", "the button names what a click spends")
	assert.Contains(t, page, "Testnet only")
}

// headAndMasthead splits off what precedes the page's masthead, the head with
// its title and styles, and the masthead itself: the strip a visitor reads
// first.
func headAndMasthead(t *testing.T, page string) (head, masthead string) {
	t.Helper()
	head, rest, found := strings.Cut(page, `<header class="masthead"`)
	require.True(t, found, "the masthead is gone")
	masthead, _, found = strings.Cut(rest, "</header>")
	require.True(t, found, "the masthead never closes")
	return head, masthead
}

// A visitor arrives from a link with no context, so before the terms the page
// says it is a demo and where its source lives.
func TestThePageOpensOnADemoMastheadLinkingTheSource(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	head, masthead := headAndMasthead(t, page)
	assert.Contains(t, head, "<title>Payment required · x402 on Gno testnet demo</title>")
	assert.Contains(t, masthead, "testnet demo", "on the page, not only in the tab")
	assert.Contains(t, masthead, `href="https://github.com/gnoverse/x402"`)
	assert.NotContains(t, head+masthead, "live", "live next to weather reads as real data")
}

// The seller answers every request with the same reading, which a visitor
// would otherwise take for the forecast they paid for.
func TestThePageSaysTheWeatherIsSampleData(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	_, masthead := headAndMasthead(t, page)
	assert.Contains(t, masthead, "sample weather data", "before paying")
	assert.Contains(t, page, "Sample data, not a forecast", "beside the paid reading")
	assert.Equal(t, "Sample weather data (demo)", termsOf(t, sellerURL).Resource.Description,
		"the only label a script or an agent reads")
}

// Adena hides the no-wallet note, so the network and where test GNOT comes
// from must sit outside it to stay on screen.
func TestThePageNamesTheNetworkAndTheFaucetWithOrWithoutAdena(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	offer, _, found := strings.Cut(page, `id="no-adena"`)
	require.True(t, found, "the no-wallet note is gone")
	start := strings.Index(offer, `<section class="offer"`)
	require.NotEqual(t, -1, start, "the offer panel is gone, or follows the no-wallet note")
	offer = offer[start:]
	assert.Contains(t, offer, "Test GNOT on <code>gno:dev</code>")
	assert.Contains(t, offer, `href="https://faucet.gno.land"`)
}

// Paying from code needs no wallet, so the offer points to it whether Adena is
// there or not, outside the blocks the script shows and hides.
func TestTheOfferPointsToPayingFromCodeWithOrWithoutAdena(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	_, offer, found := strings.Cut(page, `<section class="offer"`)
	require.True(t, found, "the offer panel is gone")
	offer, _, found = strings.Cut(offer, "</section>")
	require.True(t, found, "the offer panel never closes")
	for _, block := range []string{`<div id="pay-adena"`, `<div id="no-adena"`} {
		before, rest, found := strings.Cut(offer, block)
		require.True(t, found, "%s is gone", block)
		_, after, found := strings.Cut(rest, "</div>")
		require.True(t, found, "%s never closes", block)
		offer = before + after
	}
	assert.Contains(t, offer, `href="#code"`)
	assert.Contains(t, page, `id="code"`)
}

func TestATransactionLinksToTheExplorerOfTheChainsItKnows(t *testing.T) {
	assert.Equal(t, "https://gnoscan.io/transactions/details?chainId=onyx-1&txhash=", explorerTxURL("onyx-1"))
	assert.Empty(t, explorerTxURL("dev"), "a local chain has no explorer")
}

// The page hands the explorer to the script on the status line, and an empty
// one leaves the transaction as text.
func TestThePageCarriesNoExplorerForALocalChain(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	assert.Contains(t, page, `id="pay-status" role="status" data-tx=""`)
}

// The key step hands the buyer this page's URL, which the page's script fills
// in, so the pay command it prints needs nothing more.
func TestThePageMakesAKeyForPayingThisPage(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	assert.Contains(t, page, `--new-key</span> <span data-url>`)
}

// The pay step shows the command the key step prints: the mnemonic, the
// pinned buyer and this page's URL.
func TestThePageShowsThePayCommandTheKeyStepPrints(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	assert.Regexp(t, `<code>X402_MNEMONIC='&lt;mnemonic&gt;' npx github:gnoverse/x402#client/v\d+\.\d+\.\d+ <span data-url>`, page)
}

// The commands run the buyer from a release tag, so the script a visitor read
// is the one that runs; a branch can move between the two.
func TestThePageRunsTheBuyerPinnedToARelease(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	runs := regexp.MustCompile(`npx github:gnoverse/x402(\S*)`).FindAllStringSubmatch(page, -1)
	require.Len(t, runs, 2, "one command makes the key, one pays")
	for _, run := range runs {
		assert.Regexp(t, `^#client/v\d+\.\d+\.\d+$`, run[1])
	}
}

// The buyer signs with whatever mnemonic it is given, so the page links its
// source and warns against a key that holds anything before any command.
func TestThePageSaysToReadTheBuyerBeforeRunningIt(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	firstRun := strings.Index(page, "npx github:")
	require.NotEqual(t, -1, firstRun, "the page offers no command")
	before := page[:firstRun]
	assert.Contains(t, before, `href="https://github.com/gnoverse/x402/blob/main/examples/weather/buy.mjs"`)
	assert.Contains(t, before, "never give it a mnemonic that holds anything")
}

// The page pays through the script the seller inlines, so it must arrive
// intact: html/template escapes a string it does not know to be JavaScript.
func TestTheBrowserPageCarriesItsPaymentScriptIntact(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	assert.Contains(t, page, paywallScript)
}

// The page makes no request beyond the seller: no script, style, font or image
// from anywhere else. Links are navigation, not requests.
func TestTheBrowserPageIsSelfContained(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	for _, fetches := range []string{" src=", "<link", "<iframe", "@import", "url("} {
		assert.NotContains(t, page, fetches)
	}
	t.Logf("page: %d bytes", len(page))
}

// An id the script looks up and the page lacks is a null it throws on, and in
// the paid branch the error replaces the line that names the transaction.
func TestEveryElementThePaymentScriptFindsIsOnThePage(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	lookups := regexp.MustCompile(`getElementById\("([^"]+)"\)`).FindAllStringSubmatch(paywallScript, -1)
	require.NotEmpty(t, lookups)
	for _, lookup := range lookups {
		assert.Contains(t, page, `id="`+lookup[1]+`"`)
	}
}

// The script moves focus to the status line when the button it stood under
// goes, and a paragraph takes focus from a script only with a tabindex.
func TestTheStatusLineCanTakeFocus(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	assert.Regexp(t, `<p id="pay-status"[^>]* tabindex="-1"`, page)
}

// The script lights the hero's track step by step as a payment goes, so a step
// it names that the track lacks lights nothing.
func TestEveryStepTheScriptLightsIsOnTheTrack(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	_, track, found := strings.Cut(page, `<ol class="track"`)
	require.True(t, found, "the track is gone")
	track, _, found = strings.Cut(track, "</ol>")
	require.True(t, found, "the track never closes")
	lit := regexp.MustCompile(`report\(.*, "([0-9a-z]+)"\);|dataset\.step = "([0-9a-z]+)"`).FindAllStringSubmatch(paywallScript, -1)
	require.NotEmpty(t, lit, "paywall.js lights no step")
	for _, step := range lit {
		assert.Contains(t, track, `data-step="`+step[1]+step[2]+`"`)
	}
}

// A paid visit ends on what to do next, selling an endpoint or paying from
// code, and each way lands on its section of the page.
func TestAPaidVisitIsHandedTheNextStep(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	_, next, found := strings.Cut(page, `<nav class="next"`)
	require.True(t, found, "the next step is gone")
	next, _, found = strings.Cut(next, "</nav>")
	require.True(t, found, "the next step never closes")
	for _, section := range []string{"sell", "code"} {
		assert.Contains(t, next, `href="#`+section+`"`)
		assert.Contains(t, page, `id="`+section+`"`)
	}
}

// The script points the paid icon at #i-<forecast> for each forecast it draws,
// so a forecast without its symbol on the page parts the clouds over nothing.
func TestThePageHasAnIconForEveryForecastTheScriptDraws(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	_, page := browserPage(t, sellerURL)

	for _, forecast := range forecastsThePageDraws(t) {
		assert.Contains(t, page, `id="i-`+forecast+`"`)
	}
}

// forecastsThePageDraws returns the forecasts paywall.js has a dial and an
// icon for.
func forecastsThePageDraws(t *testing.T) []string {
	t.Helper()
	list := regexp.MustCompile(`const FORECASTS = \[([^\]]*)\]`).FindStringSubmatch(paywallScript)
	require.NotNil(t, list, "paywall.js no longer declares FORECASTS")
	var forecasts []string
	for _, quoted := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(list[1], -1) {
		forecasts = append(forecasts, quoted[1])
	}
	require.NotEmpty(t, forecasts)
	return forecasts
}

// The seller starts its turn at a random reading, so the first visitors after a
// restart do not all see the same one; the order after it is fixed.
func TestTheSellerServesItsSampleReadingsInTurnFromItsStart(t *testing.T) {
	serve := rotatingWeather([]reading{
		{Celsius: 24, Forecast: "sunny", Location: "Paris"},
		{Celsius: 14, Forecast: "rain", Location: "Paris"},
		{Celsius: 1, Forecast: "snow", Location: "Paris"},
	}, 1)

	var served []string
	for range 3 {
		rec := httptest.NewRecorder()
		serve(rec, httptest.NewRequest(http.MethodGet, weatherPath, nil))
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		served = append(served, strings.TrimSpace(rec.Body.String()))
	}

	assert.Equal(t, []string{
		`{"celsius":14,"forecast":"rain","location":"Paris"}`,
		`{"celsius":1,"forecast":"snow","location":"Paris"}`,
		`{"celsius":24,"forecast":"sunny","location":"Paris"}`,
	}, served)
}

// A reading the page cannot draw shows only as raw JSON, which the sample
// set exists to avoid.
func TestEverySampleReadingIsOneThePageDraws(t *testing.T) {
	drawn := forecastsThePageDraws(t)
	readings := sampleReadings()
	require.NotEmpty(t, readings)
	for _, r := range readings {
		assert.Contains(t, drawn, r.Forecast)
	}
}

func TestTheAmountIsShownInGNOT(t *testing.T) {
	for coin, want := range map[string][2]string{
		"250000ugnot":  {"0.25", "GNOT"},
		"1000000ugnot": {"1", "GNOT"},
		"1500000ugnot": {"1.5", "GNOT"},
		"1ugnot":       {"0.000001", "GNOT"},
		// GNOT is a name for ugnot only; another denomination is shown as is.
		"500atom": {"500", "atom"},
	} {
		t.Run(coin, func(t *testing.T) {
			amount, unit := shownAmount(std.MustParseCoin(coin))
			assert.Equal(t, want, [2]string{amount, unit})
		})
	}
}

// A browser asks for /favicon.ico on its own, beside the page; the icon is not
// what is sold, so it carries no terms.
func TestTheSellerServesItsIconUnpriced(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	resp, err := http.Get(sellerURL + "/favicon.ico")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "image/x-icon", resp.Header.Get("Content-Type"))
	assert.Equal(t, "public, max-age=86400", resp.Header.Get("Cache-Control"))
	assert.Empty(t, resp.Header.Get("PAYMENT-REQUIRED"))
	assert.Equal(t, []byte{0, 0, 1, 0}, body[:min(len(body), 4)], "an ICO file starts with this header")
}

// Someone who types the host alone lands on the demo. Without a public URL the
// redirect is relative, so it holds behind a proxy that serves the seller under
// a path ending in a slash.
func TestTheRootRedirectsToTheWeather(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get(sellerURL + "/")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "weather", resp.Header.Get("Location"))
}

// Behind a proxy that strips a prefix with no trailing slash, a relative
// Location would leave the prefix, so a seller that knows its public URL
// redirects to the weather there.
func TestTheRootRedirectsToThePublicWeather(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL, func(cfg *sellerConfig) {
		cfg.publicURL = "https://weather.example/demo"
	})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get(sellerURL + "/")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "https://weather.example/demo/weather", resp.Header.Get("Location"))
}

// TestASellerLogsWhyAPaymentWasRefused pays from an account that cannot cover
// the payment. The buyer gets the same 402 as a request that paid nothing, so
// the seller's log is the only place the refusal shows.
func TestASellerLogsWhyAPaymentWasRefused(t *testing.T) {
	txB64, account := exacttest.SignedTx(t, nil)
	node := &exacttest.Node{Account: account, SimulateErr: std.ErrInsufficientFunds("insufficient funds")}
	gnofacilitator := httptest.NewServer(facilitator.New(node, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, log := weatherSeller(t, gnofacilitator.URL)

	paid := payWith(t, sellerURL, txB64)

	require.Equal(t, http.StatusPaymentRequired, paid.StatusCode, "the payment was not refused")
	refusals := log.recordsWith(t, "reason")
	require.Len(t, refusals, 1)
	assert.Equal(t, "invalid_exact_gno_transaction_simulation_failed", refusals[0]["reason"])
	assert.Equal(t, exacttest.Payer, refusals[0]["payer"], "the signature verified, so the payer is known")
}

// TestASellerDoesNotLogAFacilitatorOutageAsARefusal stops the facilitator after
// the seller has learned what it supports. Nobody checked the payment, so the
// seller logs the facilitator's failure and names no reason against the payment.
func TestASellerDoesNotLogAFacilitatorOutageAsARefusal(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	sellerURL, log := weatherSeller(t, gnofacilitator.URL)
	gnofacilitator.Close()

	paid := payWith(t, sellerURL, exacttest.PlaceholderTx(t, nil))

	assert.Equal(t, http.StatusPaymentRequired, paid.StatusCode)
	assert.Empty(t, log.recordsWith(t, "reason"))
	assert.Len(t, log.recordsWith(t, "err"), 1, "the seller is told the facilitator failed")
}

// TestThe402NamesThePublicURL serves the seller on a loopback address, as it
// runs behind a TLS proxy. The 402 must name the URL buyers reach, or a client
// that checks it pays for a resource at an address it never asked for.
func TestThe402NamesThePublicURL(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL, func(cfg *sellerConfig) {
		cfg.publicURL = "https://weather.example"
	})

	required := termsOf(t, sellerURL)

	require.NotNil(t, required.Resource)
	assert.Equal(t, "https://weather.example/weather", required.Resource.URL)
}

func TestWithoutAPublicURLThe402NamesTheAddressItWasReachedAt(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	required := termsOf(t, sellerURL)

	require.NotNil(t, required.Resource)
	assert.Equal(t, sellerURL+"/weather", required.Resource.URL)
}

// A public URL is what every 402 names, so one that is not a plain absolute
// http(s) URL is refused at startup rather than advertised to every buyer: a
// credential in it would go out in every 402.
func TestASellerRefusesAPublicURLBuyersCannotReach(t *testing.T) {
	for _, publicURL := range []string{
		"weather.example", "ftp://weather.example", "https://", "https://weather.example?x=1",
		"https://weather.example?", "https://user:secret@weather.example",
	} {
		t.Run(publicURL, func(t *testing.T) {
			_, err := newSeller(t.Context(), sellerConfig{
				facilitatorURL: "http://127.0.0.1:1",
				network:        "gno:" + exacttest.ChainID,
				payTo:          exacttest.PayTo,
				price:          std.MustParseCoin("250000ugnot"),
				log:            slog.New(slog.DiscardHandler),
				publicURL:      publicURL,
			})
			require.ErrorContains(t, err, "-public-url")
		})
	}
}

// TestThePricedRouteIsRateLimitedBeforeThePaymentIsChecked spends the route's
// only token on an unpaid request. The paid request after it gets a 429, and the
// facilitator never hears of it: every paid request costs the facilitator a
// verify and a settle, from this seller's one address.
func TestThePricedRouteIsRateLimitedBeforeThePaymentIsChecked(t *testing.T) {
	var verifies atomic.Int32
	gno := facilitator.New(unreachableChain{}, exacttest.ChainID).Handler()
	gnofacilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/verify" {
			verifies.Add(1)
		}
		gno.ServeHTTP(w, r)
	}))
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL, func(cfg *sellerConfig) {
		cfg.ratePerSecond = 0.001
		cfg.rateBurst = 1
	})

	paid := payWith(t, sellerURL, exacttest.PlaceholderTx(t, nil))

	assert.Equal(t, http.StatusTooManyRequests, paid.StatusCode)
	assert.Zero(t, verifies.Load(), "the payment reached the facilitator")
}

// A write timeout under the time a paid request may take would cut the
// connection mid-settle: the buyer is charged and never gets the response.
func TestTheServerOutlastsAPaidRequest(t *testing.T) {
	srv := newServer(":0", http.NotFoundHandler())

	assert.Greater(t, srv.WriteTimeout, paymentTimeout)
	assert.Positive(t, srv.IdleTimeout, "an idle keep-alive connection is held forever")
	assert.Positive(t, srv.ReadTimeout, "a request body is read without a deadline")
}
