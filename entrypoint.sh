#!/bin/bash
set -e

# Start MediaMTX in background
mediamtx /app/mediamtx.yml &

# Wait for MediaMTX to be ready
sleep 2

# Start nest-rtsp
exec node /app/src/index.js "$@"
