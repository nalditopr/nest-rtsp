#!/usr/bin/with-contenv bashio
# nest-rtsp Home Assistant add-on entrypoint

CONFIG_DIR="/data"
CONFIG_FILE="${CONFIG_DIR}/config.yaml"
COOKIES_FILE="${CONFIG_DIR}/cookies.json"

# Read add-on options
bashio::log.info "Starting Nest RTSP add-on..."

# Build config.yaml from HA add-on options
{
  echo "cookies_file: ${COOKIES_FILE}"
  echo "rtsp_port: 8554"
  echo "cameras:"

  # Parse cameras from add-on config
  for camera in $(bashio::config 'cameras|keys'); do
    name=$(bashio::config "cameras[${camera}].name")
    device_id=$(bashio::config "cameras[${camera}].device_id")
    resolution=$(bashio::config "cameras[${camera}].resolution" 3)
    echo "  ${name}:"
    echo "    device_id: ${device_id}"
    echo "    resolution: ${resolution}"
  done
} > "${CONFIG_FILE}"

bashio::log.info "Config:"
cat "${CONFIG_FILE}"

# Write cookies if provided via options
cookies_json=$(bashio::config 'cookies_json')
if [ -n "${cookies_json}" ] && [ "${cookies_json}" != "null" ]; then
  echo "${cookies_json}" > "${COOKIES_FILE}"
  bashio::log.info "Cookies written from add-on config"
fi

# Check if cookies exist
if [ ! -f "${COOKIES_FILE}" ]; then
  bashio::log.warning "No cookies file found at ${COOKIES_FILE}"
  bashio::log.warning "Add cookies via the add-on configuration or paste them into ${COOKIES_FILE}"
  bashio::log.warning "See: https://github.com/nalditopr/nest-rtsp#cookie-setup"
fi

# Start nest-rtsp
bashio::log.info "Starting nest-rtsp..."
exec nest-rtsp -config "${CONFIG_FILE}"
