// The browser half of the weather seller's 402 page: it pays the resource with
// the Adena wallet the way a stock x402 client does, shows the live terms, and
// writes the weather it bought into the page's sky.
// The seller inlines this file into the page, so it stays one self-contained
// module: no imports, no build step, no request beyond the seller itself.

const NETWORK_PREFIX = "gno:";

// tm2's denomination grammar, as the client mechanism checks it: the coin sent
// is amount + asset as one string, so an asset outside it signs a different
// coin.
const DENOMINATION = /^[a-z/][a-z0-9_.:/-]{2,273}$/;
const AMOUNT = /^[1-9][0-9]*$/;

// The longest memo a payment may carry, in UTF-8 bytes, as the facilitator
// counts it.
const MAX_MEMO_BYTES = 256;

/** Encodes value as an x402 header carries it: base64 of its JSON, UTF-8. */
export function encodeHeader(value) {
  const bytes = new TextEncoder().encode(JSON.stringify(value));
  return btoa(Array.from(bytes, (b) => String.fromCharCode(b)).join(""));
}

/** Decodes an x402 header back to its JSON value. */
export function decodeHeader(header) {
  const bytes = Uint8Array.from(atob(header), (c) => c.charCodeAt(0));
  return JSON.parse(new TextDecoder().decode(bytes));
}

// Asking for JSON is what makes the seller answer with the 402's headers
// rather than with this page again.
async function request(fetchFn, url, headers = {}) {
  return fetchFn(url, { headers: { Accept: "application/json", ...headers } });
}

/** Returns the terms the seller answers an unpaid request with. */
export async function readTerms(fetchFn, url) {
  const response = await request(fetchFn, url);
  const header = response.headers.get("PAYMENT-REQUIRED");
  if (response.status !== 402 || header === null) {
    throw new Error(`the seller answered ${response.status} without payment terms`);
  }
  return decodeHeader(header);
}

// firstText returns the first of values that is a non-empty string, or "".
function firstText(...values) {
  return values.find((v) => typeof v === "string" && v !== "") ?? "";
}

// The gno entry of the offer and the memo its payment carries, checked the way
// the client mechanism checks them before anything is signed. A key of extra
// set to null names nothing, as upstream's SDKs read it.
function gnoOffer(terms) {
  const accepted = (terms.accepts ?? []).find(
    (a) => a.scheme === "exact" && typeof a.network === "string" && a.network.startsWith(NETWORK_PREFIX),
  );
  if (accepted === undefined) {
    throw new Error("this resource offers no gno payment");
  }
  if (!AMOUNT.test(accepted.amount) || !DENOMINATION.test(accepted.asset)) {
    throw new Error(`${accepted.amount} ${accepted.asset} is not a gno coin`);
  }
  if (typeof accepted.payTo !== "string" || accepted.payTo === "") {
    throw new Error("the offer names no payee");
  }
  const method = accepted.extra?.assetTransferMethod;
  if (method !== undefined && method !== null && method !== "bank-send") {
    throw new Error(`the offer pays by ${String(method)}, not by a bank send`);
  }
  const flow = accepted.extra?.paymentFlow;
  if (flow !== undefined && flow !== null && flow !== "authorization") {
    throw new Error(`the offer's payment flow is ${String(flow)}, not authorization`);
  }
  const memo = accepted.extra?.memo;
  if (memo === undefined) {
    return { accepted, memo: "" };
  }
  if (typeof memo !== "string") {
    throw new Error(`the offer's memo is a ${typeof memo}, not text`);
  }
  const bytes = new TextEncoder().encode(memo).length;
  if (bytes > MAX_MEMO_BYTES) {
    throw new Error(`the offer's memo is ${bytes} bytes, over the ${MAX_MEMO_BYTES} a payment can carry`);
  }
  return { accepted, memo };
}

// Adena answers ALREADY_CONNECTED and REDUNDANT_CHANGE_REQUEST as failures
// although they mean the wallet is already where the page wants it.
function expectSuccess(response, alsoFine = []) {
  if (response?.status === "success" || alsoFine.includes(response?.type)) {
    return response.data;
  }
  const said = firstText(response?.message, response?.type);
  throw new Error(`Adena: ${said === "" ? "no answer" : said}`);
}

/**
 * Pays url with Adena: reads the terms, has Adena sign a bank send of the
 * amount to the payee on the offer's chain without broadcasting it, and
 * repeats the request with that payment.
 *
 * Throws before anything is sent for a wallet refusal, a missing network or an
 * offer the page cannot pay, and after sending when no answer came back.
 * Otherwise returns what the paid request answered.
 *
 * report hears each stage with the step of the exchange it belongs to: "402"
 * while the terms are read, "sign" while Adena connects and signs, "retry"
 * while the paid request goes out.
 */
export async function payWithAdena({ adena, fetch: fetchFn, url, report = () => {} }) {
  report("Reading the terms…", "402");
  const terms = await readTerms(fetchFn, url);
  const { accepted, memo } = gnoOffer(terms);
  const chainId = accepted.network.slice(NETWORK_PREFIX.length);

  report("Connecting to Adena…", "sign");
  expectSuccess(await adena.AddEstablish("x402 weather demo"), ["ALREADY_CONNECTED"]);
  const network = expectSuccess(await adena.GetNetwork());
  if (network.chainId !== chainId) {
    report(`Switching Adena to ${chainId}…`, "sign");
    const switched = await adena.SwitchNetwork(chainId);
    if (switched?.type === "UNADDED_NETWORK") {
      throw new Error(`Adena has no network ${chainId}; add it in Adena, then pay again`);
    }
    expectSuccess(switched, ["REDUNDANT_CHANGE_REQUEST"]);
  }
  const account = expectSuccess(await adena.GetAccount());

  report("Confirm the payment in Adena…", "sign");
  const signed = expectSuccess(
    await adena.SignTx({
      messages: [
        {
          type: "/bank.MsgSend",
          value: {
            from_address: account.address,
            to_address: accepted.payTo,
            amount: `${accepted.amount}${accepted.asset}`,
          },
        },
      ],
      memo,
    }),
  );

  report("Paying…", "retry");
  const payment = {
    x402Version: terms.x402Version,
    resource: terms.resource,
    accepted,
    payload: { transaction: signed.encodedTransaction },
  };
  let response;
  let body;
  try {
    response = await request(fetchFn, url, { "PAYMENT-SIGNATURE": encodeHeader(payment) });
    body = await response.text();
  } catch (err) {
    const cause = err instanceof Error ? err.message : String(err);
    throw new Error(`The payment was sent, but no answer came back (${cause}). Check the account before paying again.`);
  }

  // A settle failure carries its reason in the receipt, a verify refusal in a
  // fresh challenge.
  const receiptHeader = response.headers.get("PAYMENT-RESPONSE");
  const challengeHeader = response.headers.get("PAYMENT-REQUIRED");
  const receipt = receiptHeader === null ? {} : decodeHeader(receiptHeader);
  const challenge = challengeHeader === null ? {} : decodeHeader(challengeHeader);
  return {
    paid: response.ok && receipt.success === true,
    status: response.status,
    body,
    transaction: receipt.transaction ?? "",
    reason: firstText(receipt.errorReason, challenge.error),
  };
}

/**
 * Says what a payment came to. A payment the chain admitted but has not yet
 * executed comes back unpaid with its transaction, and may still go through.
 */
export function statusLine(result) {
  if (result.paid) {
    return `Paid. Transaction ${result.transaction}.`;
  }
  if (result.transaction !== "") {
    return `Sent, not confirmed yet (${result.reason}): the weather was withheld, but transaction ${result.transaction} may still go through.`;
  }
  return `Not paid (${result.status})${result.reason === "" ? "" : `: ${result.reason}`}`;
}

// The forecasts the page has a sky and an icon for; the icon of each is the
// symbol #i-<forecast>.
const FORECASTS = ["sunny", "cloudy", "rain", "snow"];

// readingOf returns the reading the sky can draw from a paid body, or null for
// any other body, which the page then shows only as served.
function readingOf(body) {
  let weather;
  try {
    weather = JSON.parse(body);
  } catch {
    return null;
  }
  if (typeof weather?.celsius !== "number" || !FORECASTS.includes(weather.forecast)) {
    return null;
  }
  return { celsius: weather.celsius, forecast: weather.forecast };
}

// How long the page waits for Adena to inject window.adena: it does so from a
// script it loads itself, which can land after the page's load event.
const ADENA_WAIT_MS = 5000;
const ADENA_POLL_MS = 250;

/** Wires the page: this page's URL, copy buttons, the live terms, and Adena. */
export function start(document, window) {
  // A fragment comes from a link inside this page, not from the seller.
  const { origin, pathname, search } = window.location;
  const here = origin + pathname + search;
  for (const e of document.querySelectorAll("[data-url]")) {
    e.textContent = here;
  }

  if (window.navigator.clipboard !== undefined) {
    for (const pre of document.querySelectorAll("pre[data-copy]")) {
      const button = document.createElement("button");
      button.type = "button";
      button.className = "copy";
      button.textContent = "Copy";
      button.addEventListener("click", async () => {
        try {
          await window.navigator.clipboard.writeText(pre.querySelector("code").innerText);
          button.textContent = "Copied";
        } catch {
          button.textContent = "Copy failed";
        }
        window.setTimeout(() => (button.textContent = "Copy"), 1500);
      });
      pre.append(button);
    }
  }

  const terms = document.getElementById("terms");
  let termsRead = false;
  let termsReading = false;
  terms.addEventListener("toggle", async () => {
    if (termsRead || termsReading || !terms.open) {
      return;
    }
    const out = document.getElementById("terms-json");
    termsReading = true;
    try {
      out.textContent = JSON.stringify(await readTerms(window.fetch.bind(window), here), null, 2);
      termsRead = true;
    } catch (err) {
      out.textContent = String(err);
    } finally {
      termsReading = false;
    }
  });

  // Presence alone is not the wallet: an element whose id is "adena" would be
  // window.adena too.
  const offerAdena = () => {
    if (typeof window.adena?.SignTx !== "function") {
      return false;
    }
    document.getElementById("no-adena").hidden = true;
    document.getElementById("pay-adena").hidden = false;
    return true;
  };
  if (!offerAdena()) {
    let waited = 0;
    const poll = window.setInterval(() => {
      waited += ADENA_POLL_MS;
      if (offerAdena() || waited >= ADENA_WAIT_MS) {
        window.clearInterval(poll);
      }
    }, ADENA_POLL_MS);
  }

  const button = document.getElementById("pay");
  const status = document.getElementById("pay-status");
  const result = document.getElementById("pay-result");
  // The sky's data-step lights the hero's track; without one it rests on the
  // unpaid request.
  const sky = document.getElementById("sky");
  button.addEventListener("click", async () => {
    button.disabled = true;
    result.hidden = true;
    try {
      const paid = await payWithAdena({
        adena: window.adena,
        fetch: window.fetch.bind(window),
        url: here,
        report: (message, step) => {
          status.textContent = message;
          sky.dataset.step = step;
        },
      });
      status.textContent = statusLine(paid);
      if (paid.paid) {
        sky.dataset.step = "200";
        // data-tx is the chain's explorer page for a transaction, less its hash;
        // empty on a chain without one.
        if (status.dataset.tx) {
          const link = document.createElement("a");
          link.setAttribute("href", status.dataset.tx + encodeURIComponent(paid.transaction));
          link.textContent = paid.transaction;
          status.replaceChildren("Paid. Transaction ", link, ".");
        }
        button.hidden = true;
        result.querySelector("code").textContent = paid.body;
        result.hidden = false;
        const reading = readingOf(paid.body);
        if (reading !== null) {
          document.getElementById("paid-temp").textContent = String(reading.celsius);
          document.getElementById("paid-forecast").textContent = reading.forecast;
          document.getElementById("paid-icon").setAttribute("href", `#i-${reading.forecast}`);
          sky.dataset.forecast = reading.forecast;
        }
      } else {
        delete sky.dataset.step;
      }
    } catch (err) {
      status.textContent = err instanceof Error ? err.message : String(err);
      delete sky.dataset.step;
    } finally {
      button.disabled = false;
    }
  });
}
