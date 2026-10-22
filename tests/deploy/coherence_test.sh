#!/usr/bin/env bash
#
# Starts coherence from the compose file on a fresh, owner-only (0700) data
# dir, as setup-host.sh creates it, and checks the home page is served.
#
# Regression: coherence runs as uid 1600 in its container and could not read
# its 0700 volume (owned by the aejar user), and on a fresh install it had no
# doc dir or home index, so every page was a 404. No manual step may be needed.
set -euo pipefail
cd "$(dirname "$0")/../.."
proj=aejar-coh-test-$$
data=$(mktemp -d)
cleanup() {
    docker compose -p "$proj" -f deploy/compose.yaml --env-file "$data/env" down -v >/dev/null 2>&1 || true
    docker run --rm -v "$data:/d" alpine:3.20 rm -rf /d/coherence /d/secrets >/dev/null 2>&1 || true
    rm -rf "$data"
}
trap cleanup EXIT
mkdir -p "$data/coherence" "$data/secrets"
chmod 700 "$data/coherence"
: > "$data/secrets/oauth2-proxy.env"
cat > "$data/env" <<ENV
AEJAR_DATA=$data
DOCKER_SOCK=/var/run/docker.sock
AEJAR_HOST=aejar.example.com
DOCS_HOST=docs.example.com
ENV
docker compose -p "$proj" -f deploy/compose.yaml --env-file "$data/env" up -d --no-build coherence >/dev/null 2>&1
net=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$proj-coherence-1" | awk '{print $1}')
code=000
for _ in $(seq 1 20); do
    code=$(docker run --rm --network "$net" curlimages/curl:8.11.1 -s -o /dev/null -w '%{http_code}' http://coherence:8080/ || true)
    [[ "$code" == "200" ]] && break
    sleep 1
done
if [[ "$code" == "200" ]]; then echo "ok   coherence home page: $code"; else echo "FAIL coherence home page: $code"; exit 1; fi
