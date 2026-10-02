#!/bin/sh
# Signs a release for `hakobu update` and install.sh, run by GoReleaser on
# checksums.txt: the signature covers "hakobu <tag>\n" followed by the
# checksums, so an older release can't be passed off as a newer one. The
# Ed25519 key comes in HAKOBU_SIGNING_KEY: its PEM file, or just the
# base64 line of it. Its public half is in scripts/release-key.pub,
# internal/update and install.sh.
#
#   sign-release.sh <tag> <checksums.txt> <signature>
set -eu

tag=$1
artifact=$2
signature=$3
if [ -z "${HAKOBU_SIGNING_KEY:-}" ]; then
  echo "HAKOBU_SIGNING_KEY isn't set: a release is never published unsigned" >&2
  exit 1
fi
msg=$(mktemp)
trap 'rm -f "$msg"' EXIT
{ printf 'hakobu %s\n' "$tag"; cat "$artifact"; } > "$msg"
case "$HAKOBU_SIGNING_KEY" in
  -----BEGIN*) key=$HAKOBU_SIGNING_KEY ;;
  *) key=$(printf -- '-----BEGIN PRIVATE KEY-----\n%s\n-----END PRIVATE KEY-----' "$HAKOBU_SIGNING_KEY") ;;
esac
printf '%s\n' "$key" | openssl pkeyutl -sign -rawin -inkey /dev/stdin -in "$msg" -out "$signature"
# A key that isn't the one servers trust would make a release no server
# installs: fail here instead.
openssl pkeyutl -verify -rawin -pubin -inkey "$(dirname "$0")/release-key.pub" -in "$msg" -sigfile "$signature" >/dev/null
