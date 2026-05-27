#!/usr/bin/env sh
set -eu

cd /opt/spring-code-1

docker run --rm \
  -p 80:80 \
  -v /opt/spring-code-1/deploy/letsencrypt:/etc/letsencrypt \
  -v /opt/spring-code-1/deploy/certbot-www:/var/lib/letsencrypt \
  certbot/certbot renew --standalone

docker compose restart scanner-proxy
