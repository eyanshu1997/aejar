#!/usr/bin/env bash
#
# Runs the real nginx template against a stub oauth2-proxy that always says
# "not signed in" (401), and checks what an anonymous browser gets.
#
# Regression: the sign-in redirect was absolute and built from nginx's own
# listener (http://host:9011/...), dropping https and leaking the internal
# port, so browsers left the tunnel. It must be a relative Location.
set -euo pipefail
cd "$(dirname "$0")/../.."
net=aejar-nginx-test-$$
cleanup() { docker rm -f "$net-stub" "$net-nginx" >/dev/null 2>&1; docker network rm "$net" >/dev/null 2>&1; }
trap cleanup EXIT
docker network create "$net" >/dev/null
# Stub oauth2-proxy (and the upstream apps): /oauth2/auth answers 401.
docker run -d --name "$net-stub" --network "$net" --network-alias oauth2-proxy \
    --network-alias aejard --network-alias coherence nginx:1.31.8-alpine sh -c \
    'printf "server { listen 4180; location / { return 401; } }\nserver { listen 9111; location / { return 200 app; } }\nserver { listen 8080; location / { return 200 docs; } }\n" > /etc/nginx/conf.d/default.conf && nginx -g "daemon off;"' >/dev/null
docker run -d --name "$net-nginx" --network "$net" -e AEJAR_HOST=aejar.example.com -e DOCS_HOST=docs.example.com \
    -v "$PWD/deploy/nginx/templates:/etc/nginx/templates:ro" \
    -v "$PWD/deploy/nginx/aejar-auth.inc:/etc/nginx/conf.d/aejar-auth.inc:ro" nginx:1.31.8-alpine >/dev/null
sleep 2

fail=0
check() { # port host path expected-location-prefix
    loc=$(docker run --rm --network "$net" curlimages/curl:8.11.1 -s -o /dev/null -D - \
        -H "Host: $2" -H "X-Id-Token: Bearer forged" "http://$net-nginx:$1$3" | sed -n 's/^[Ll]ocation: //p' | tr -d '\r')
    if [[ "$loc" == "$4"* ]]; then echo "ok   $2$3 -> $loc"; else echo "FAIL $2$3 -> '$loc' (want prefix $4)"; fail=1; fi
}
check 9011 aejar.example.com /v1/tasks "/oauth2/start?rd=https://aejar.example.com/v1/tasks"
check 9010 docs.example.com /plan.html "/oauth2/start?rd=https://docs.example.com/plan.html"
exit $fail
