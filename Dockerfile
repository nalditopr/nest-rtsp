FROM golang:1.24-bookworm AS go-builder
WORKDIR /build
COPY pion-bridge/go.mod pion-bridge/go.sum ./
RUN go mod download
COPY pion-bridge/main.go .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o pion-bridge .

FROM node:22-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg \
    chromium \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY package.json .
RUN npm install --production

COPY --from=go-builder /build/pion-bridge /app/pion-bridge/pion-bridge
COPY src/ src/
COPY mediamtx.yml .
COPY config.example.yaml .

# Download MediaMTX
RUN apt-get update && apt-get install -y --no-install-recommends wget && \
    wget -q https://github.com/bluenviron/mediamtx/releases/download/v1.9.3/mediamtx_v1.9.3_linux_amd64.tar.gz -O /tmp/mediamtx.tar.gz && \
    tar -xzf /tmp/mediamtx.tar.gz -C /usr/local/bin mediamtx && \
    rm /tmp/mediamtx.tar.gz && \
    apt-get remove -y wget && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*

VOLUME /data
EXPOSE 8554 3000 9997

ENV COOKIES_FILE=/data/cookies.json
ENV CHROME_PROFILE=/data/chrome-profile
ENV CHROMIUM_PATH=/usr/bin/chromium
ENV COOKIE_REFRESH_HOURS=6
ENV RTSP_PORT=8554
ENV COOKIE_SERVER_PORT=3000
ENV STREAM_MODE=pion

COPY entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

CMD ["/app/entrypoint.sh"]
