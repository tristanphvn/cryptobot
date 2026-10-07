#!/usr/bin/env bash
# One-command setup: ./scripts/setup.sh [--skip-model] [--no-start]
#
#   1. checks Docker (required) and Go / Node (optional, for local dev)
#   2. creates .env from .env.example with a random DB password and
#      operator token (an existing .env is never overwritten)
#   3. downloads Go modules and npm packages when Go / Node are present
#   4. builds and starts the stack with docker compose
#   5. pulls the Ollama model and waits until the dashboard answers
#
# Safe to re-run. Everything stays bound to 127.0.0.1 by default.
set -euo pipefail

SKIP_MODEL=0
START=1
for arg in "$@"; do
  case "$arg" in
    --skip-model) SKIP_MODEL=1 ;;
    --no-start) START=0 ;;
    -h|--help) sed -n '2,11p' "$0"; exit 0 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok()   { printf '    \033[32m✓\033[0m %s\n' "$*"; }
warn() { printf '    \033[33m!\033[0m %s\n' "$*"; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

random_hex() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$1"
  else
    head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

# Value of KEY as docker compose sees it: an exported shell variable wins
# over .env (last assignment), then the default.
env_value() {
  local v="${!1:-}"
  [ -n "$v" ] || v="$(grep -E "^$1=" .env 2>/dev/null | tail -n1 | cut -d= -f2- || true)"
  printf '%s' "${v:-$2}"
}

step "Checking prerequisites"
command -v docker >/dev/null 2>&1 || die "Docker is required: https://docs.docker.com/get-docker/"
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required (docker compose ...)"
docker info >/dev/null 2>&1 || die "Docker is installed but not running — start Docker Desktop / the daemon"
ok "docker $(docker version --format '{{.Server.Version}}'), $(docker compose version --short)"
HAVE_GO=0; HAVE_NODE=0
if command -v go >/dev/null 2>&1; then HAVE_GO=1; ok "$(go version | cut -d' ' -f3) (toolchain go1.26.6 auto-downloads if needed)"; else warn "Go not found — skipped (only needed to run the backend outside docker)"; fi
if command -v node >/dev/null 2>&1; then HAVE_NODE=1; ok "node $(node --version)"; else warn "Node not found — skipped (only needed to run the frontend outside docker)"; fi

step "Environment file"
if [ -f .env ]; then
  ok ".env exists — left untouched"
else
  [ -f .env.example ] || die ".env.example is missing"
  cp .env.example .env
  pw="$(random_hex 24)"
  token="$(random_hex 32)"
  # Portable in-place edit (GNU and BSD sed differ on -i).
  tmp="$(mktemp)"
  sed -e "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$pw|" \
      -e "s|^APPROVAL_TOKEN=.*|APPROVAL_TOKEN=$token|" .env > "$tmp" && mv "$tmp" .env
  chmod 600 .env 2>/dev/null || true
  ok "created .env with a random POSTGRES_PASSWORD and APPROVAL_TOKEN"
fi

if [ "$HAVE_GO" = 1 ]; then
  step "Go modules"
  (cd backend && go mod download && go build ./...) && ok "backend builds"
fi
if [ "$HAVE_NODE" = 1 ]; then
  step "npm packages"
  (cd frontend && npm ci --no-audit --no-fund) && ok "frontend dependencies installed"
fi

# Fail early with the variable to change instead of a docker bind error.
port_busy() {
  if command -v nc >/dev/null 2>&1; then nc -z 127.0.0.1 "$1" >/dev/null 2>&1
  else (exec 3<>"/dev/tcp/127.0.0.1/$1") >/dev/null 2>&1; fi
}
if [ "$START" = 1 ] && [ -z "$(docker compose ps -q 2>/dev/null)" ]; then
  step "Checking host ports"
  for pair in BACKEND_PORT:8081 FRONTEND_PORT:3002 POSTGRES_PORT:5432 OLLAMA_PORT:11434; do
    var="${pair%%:*}"; port="$(env_value "$var" "${pair##*:}")"
    if port_busy "$port"; then die "port $port is in use — set $var to a free port in .env and re-run"; fi
  done
  ok "ports free"
fi

if [ "$START" = 0 ]; then
  step "Done (--no-start)"; echo "    Start later with: docker compose up -d --build"; exit 0
fi

step "Building and starting the stack (first build takes a few minutes)"
docker compose up -d --build
ok "containers started"

MODEL="$(env_value OLLAMA_MODEL llama3)"
if [ "$SKIP_MODEL" = 0 ]; then
  step "Pulling Ollama model '$MODEL' (~4-5 GB the first time)"
  until docker compose exec -T ollama ollama list >/dev/null 2>&1; do sleep 2; done
  docker compose exec -T ollama ollama pull "$MODEL"
  ok "model ready"
else
  warn "model pull skipped — run: docker compose exec ollama ollama pull $MODEL"
fi

BACKEND_PORT="$(env_value BACKEND_PORT 8081)"
FRONTEND_PORT="$(env_value FRONTEND_PORT 3002)"
step "Waiting for the services"
for url in "http://127.0.0.1:$BACKEND_PORT/health" "http://127.0.0.1:$FRONTEND_PORT/"; do
  for _ in $(seq 1 90); do
    if curl -fsS -o /dev/null "$url" 2>/dev/null; then ok "$url"; continue 2; fi
    sleep 2
  done
  die "$url did not answer — check: docker compose logs --tail=100"
done

MODE="$(env_value EXECUTION_MODE dry-run)"
if [ "$MODE" = live ]; then NOTE="LIVE — real orders on Binance"; else NOTE="no real orders"; fi
cat <<EOF

  CryptoBot is running in $MODE mode ($NOTE).

    Dashboard  http://localhost:$FRONTEND_PORT
    API        http://localhost:$BACKEND_PORT/health

  Operator token (paste into Settings / the approval card): see APPROVAL_TOKEN in .env
  Stop: docker compose down      Logs: docker compose logs -f backend
EOF
