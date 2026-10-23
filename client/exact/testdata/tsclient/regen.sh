#!/usr/bin/env bash
# Regenerates the TypeScript-signed fixtures the Go mechanism's interop test
# compares against. The fixtures are committed so the Go test needs no
# toolchain; this script is only for changing what either mechanism signs.
set -o errexit
set -o nounset
set -o pipefail

# %/* strips the last path segment, which yields nothing to cd to when the script
# was named without one — `bash regen.sh` from this directory. cd to the script's
# own directory whichever way it was invoked.
HERE="${BASH_SOURCE[0]%/*}"
[[ "${HERE}" == "${BASH_SOURCE[0]}" ]] && HERE="."
readonly HERE
cd "${HERE}"

if ! command -v npm > /dev/null; then
  echo "Error: npm is required to regenerate the TypeScript fixtures" >&2
  exit 1
fi

# sign.mjs imports the mechanism by package name, which resolves to the built
# client/dist through the root manifest's exports map, so build it first.
make -C ../../../.. js

# Each fixture is written aside and moved into place: redirecting straight at it
# truncates it before node runs, so a failure would leave an empty fixture where
# a committed one was. Each gets a temporary file of its own, made beside its
# destination so the move is a rename, and readable like the files git checks
# out.
for kind in master session; do
  fixture="../tsclient_${kind}_payment.b64"
  tmp="$(mktemp "${fixture}.XXXXXX")"
  trap 'rm -f "${tmp}"' EXIT
  node sign.mjs "${kind}" > "${tmp}"
  chmod 0644 "${tmp}"
  mv "${tmp}" "${fixture}"
done
