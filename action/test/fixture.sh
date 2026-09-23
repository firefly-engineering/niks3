#!/usr/bin/env bash
# Starts a private niks3 cache on the runner, laid out like a production
# deployment: Caddy in front, /api/* passed straight to niks3 (which checks
# GitHub OIDC tokens itself), every other path behind HTTP Basic auth.
#
# Usage: fixture.sh <state-dir> <basic-login> <basic-password>
# Prints KEY=VALUE lines for $GITHUB_OUTPUT: url, public-key, bin (a directory
# holding niks3 and niks3-hook built from this checkout).
set -euo pipefail

state=$1
login=$2
password=$3
root=$(cd "$(dirname "$0")/../.." && pwd)
mkdir -p "$state"/{pg,s3}

build() { nix build --quiet --no-link --print-out-paths --inputs-from "$root" "$@"; }

server=$(build "$root#niks3-server")
client=$(build "$root#niks3")
hook=$(build "$root#niks3-hook")
pg=$(build nixpkgs#postgresql_16.out)
rustfs=$(build nixpkgs#rustfs)
caddy=$(build nixpkgs#caddy)
s5cmd=$(build nixpkgs#s5cmd)

mkdir -p "$state/bin"
ln -sf "$client/bin/niks3" "$hook/bin/niks3-hook" "$state/bin/"

wait_for() {
  for _ in $(seq 120); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  echo "timed out waiting for: $*" >&2
  return 1
}

# Postgres on a unix socket only.
"$pg/bin/initdb" -D "$state/pg" -U postgres --auth=trust >/dev/null
"$pg/bin/pg_ctl" -D "$state/pg" -l "$state/pg.log" -o "-k $state -c listen_addresses=''" start >/dev/null

# S3.
"$rustfs/bin/rustfs" --address 127.0.0.1:9000 --console-address 127.0.0.1:9001 \
  --access-key rustfsadmin --secret-key rustfsadmin "$state/s3" >"$state/rustfs.log" 2>&1 &
wait_for curl -sf http://127.0.0.1:9000/health/ready
S3_ENDPOINT_URL=http://127.0.0.1:9000 AWS_ACCESS_KEY_ID=rustfsadmin AWS_SECRET_ACCESS_KEY=rustfsadmin \
  "$s5cmd/bin/s5cmd" mb s3://niks3 >/dev/null

nix key generate-secret --key-name niks3-e2e-1 >"$state/sign.key"
pubkey=$(nix key convert-secret-to-public <"$state/sign.key")

# Writes: GitHub OIDC tokens of this repository only.
cat >"$state/oidc.json" <<EOF
{
  "providers": {
    "github": {
      "issuer": "https://token.actions.githubusercontent.com",
      "audience": "niks3-e2e",
      "bound_claims": { "repository": ["${GITHUB_REPOSITORY:-firefly-engineering/niks3}"] },
      "scopes": ["write"]
    }
  }
}
EOF

NIKS3_DB="host=$state user=postgres dbname=postgres sslmode=disable" \
  "$server/bin/niks3-server" \
  --http-addr 127.0.0.1:5751 \
  --s3-endpoint 127.0.0.1:9000 --s3-use-ssl=false --s3-bucket niks3 \
  --s3-access-key rustfsadmin --s3-secret-key rustfsadmin \
  --api-token "$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')" \
  --oidc-config "$state/oidc.json" \
  --sign-key-path "$state/sign.key" \
  --cache-url http://127.0.0.1:8080 \
  --enable-read-proxy \
  --debug >"$state/niks3-server.log" 2>&1 &
wait_for curl -sf http://127.0.0.1:5751/health

hash=$("$caddy/bin/caddy" hash-password --plaintext "$password")
cat >"$state/Caddyfile" <<EOF
{
  admin off
}
http://127.0.0.1:8080 {
  log {
    output file $state/caddy-access.log
  }
  @private not path /api/*
  basic_auth @private {
    $login $hash
  }
  reverse_proxy 127.0.0.1:5751
}
EOF
"$caddy/bin/caddy" run --config "$state/Caddyfile" --adapter caddyfile >"$state/caddy.log" 2>&1 &
wait_for curl -sf http://127.0.0.1:8080/api/cache-config

echo "url=http://127.0.0.1:8080"
echo "public-key=$pubkey"
echo "bin=$state/bin"
