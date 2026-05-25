#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/metrics | head -n 20
curl -fsS http://127.0.0.1:9090/-/healthy

echo "Server check completed."
