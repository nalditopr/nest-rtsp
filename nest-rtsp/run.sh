#!/bin/bash
# nest-rtsp Home Assistant add-on

CONFIG_DIR="/data"
CONFIG_FILE="${CONFIG_DIR}/config.yaml"
COOKIES_FILE="${CONFIG_DIR}/cookies.json"
OPTIONS="/data/options.json"

echo "[nest-rtsp] Starting..."

# Parse HA add-on options.json into our config format
if [ -f "${OPTIONS}" ]; then
  rtsp_port=$(jq -r '.rtsp_port // 8555' "${OPTIONS}")
  {
    echo "cookies_file: ${COOKIES_FILE}"
    echo "rtsp_port: ${rtsp_port}"
    echo "cameras:"

    # Parse cameras array from options.json
    camera_count=$(jq '.cameras | length' "${OPTIONS}")
    for i in $(seq 0 $((camera_count - 1))); do
      name=$(jq -r ".cameras[$i].name" "${OPTIONS}")
      device_id=$(jq -r ".cameras[$i].device_id" "${OPTIONS}")
      resolution=$(jq -r ".cameras[$i].resolution // 3" "${OPTIONS}")
      echo "  ${name}:"
      echo "    device_id: ${device_id}"
      echo "    resolution: ${resolution}"
    done
  } > "${CONFIG_FILE}"

  echo "[nest-rtsp] Config:"
  cat "${CONFIG_FILE}"

  # Write cookies from options
  cookies_json=$(jq -r '.cookies_json // empty' "${OPTIONS}")
  if [ -n "${cookies_json}" ]; then
    echo "${cookies_json}" > "${COOKIES_FILE}"
    echo "[nest-rtsp] Cookies written ($(echo "${cookies_json}" | wc -c) bytes)"
  fi
else
  echo "[nest-rtsp] No options.json found, using existing config"
fi

if [ ! -f "${COOKIES_FILE}" ]; then
  echo "[nest-rtsp] WARNING: No cookies file — add cookies via Configuration tab"
fi

# Run the Go binary — stdout/stderr go directly to Docker logs
echo "[nest-rtsp] Starting nest-rtsp binary..."
exec nest-rtsp -config "${CONFIG_FILE}"
