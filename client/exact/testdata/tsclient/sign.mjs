// Signs the payments the Go mechanism's interop test compares itself against,
// with the TypeScript mechanism in this repository, and prints the base64
// transaction its payload carries.
//
//   node sign.mjs master    pays from test1's own account
//   node sign.mjs session   pays from test1's account with a session key
//
// The mechanism is imported by package name, so it is the built client/dist a
// buyer installing the package would run, not its sources. The provider answers
// only what a payment reads — the account state and the gas price — with the
// numbers the Go test's fake node answers, so no node is contacted and the
// output is reproducible.
import { GnoWallet } from "@gnolang/gno-js-client";

import { ExactGnoScheme } from "@gnoverse/x402-gno/exact/client";

// gno's well-known test1 key, the payer.
const TEST1 =
  "source bonus chronic canvas draft south burst lottery vacant surface solve popular case indicate oppose farm nothing bullet exhibit title speed wink action roast";
const TEST1_ADDRESS = "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5";

// A session key, public like test1's and never funded.
const SESSION =
  "dance carry grunt uphold cloth wire usage usage clock reduce surprise page destroy reopen multiply away defense promote person web cigar immense armed enough";

const REQUIREMENTS = {
  scheme: "exact",
  network: "gno:dev",
  asset: "ugnot",
  amount: "250000",
  payTo: "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq",
  maxTimeoutSeconds: 300,
  extra: { areFeesSponsored: false, memo: "x402-interop" },
};

// The chain as the Go test's fake node describes it: test1's account at number
// 12, sequence 7; the session account under it at 34, 3; onyx-1's gas price.
const provider = {
  getAccount: async (address) => ({
    BaseAccount: { address, coins: "", public_key: null, account_number: "12", sequence: "7" },
  }),
  getSession: async (master, session) => ({
    address: session,
    master_address: master,
    allow_paths: ["bank/send"],
    account_number: "34",
    sequence: "3",
  }),
  getGasPrice: async () => ({ amount: 1, denom: "ugnot", gas: 1000 }),
};

const kind = process.argv[2];
if (kind !== "master" && kind !== "session") {
  console.error("usage: node sign.mjs master|session");
  process.exit(2);
}

const wallet = await GnoWallet.fromMnemonic(kind === "master" ? TEST1 : SESSION);
wallet.connect(provider);
const scheme = new ExactGnoScheme(wallet, kind === "master" ? {} : { master: TEST1_ADDRESS });
const { payload } = await scheme.createPaymentPayload(2, REQUIREMENTS);

console.log(payload.transaction);
