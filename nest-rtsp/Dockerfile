ARG BUILD_FROM=ghcr.io/hassio-addons/base:17.2.5
FROM golang:1.25rc1-bookworm AS builder
ENV GOTOOLCHAIN=auto

WORKDIR /build
COPY nest-rtsp-go/go.mod nest-rtsp-go/go.sum ./
RUN go mod download
COPY nest-rtsp-go/main.go .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o nest-rtsp .

FROM ${BUILD_FROM}

COPY --from=builder /build/nest-rtsp /usr/local/bin/nest-rtsp
COPY run.sh /etc/services.d/nest-rtsp/run
RUN chmod +x /etc/services.d/nest-rtsp/run
