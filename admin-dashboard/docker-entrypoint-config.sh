#!/bin/sh
# Runs at container start (nginx image runs /docker-entrypoint.d/*.sh): writes config.js from API_URL,
# so the API URL can be changed without rebuilding the image or touching the source.
set -eu
: "${API_URL:=http://localhost:8080}"
printf 'window.APP_CONFIG = { API_URL: "%s" };\n' "$API_URL" > /usr/share/nginx/html/config.js
echo "config.js written: API_URL=$API_URL"
