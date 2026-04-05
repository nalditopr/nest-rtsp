FROM golang:1.25rc1-bookworm AS builder
ENV GOTOOLCHAIN=auto
WORKDIR /build
COPY nest-rtsp-go/go.mod nest-rtsp-go/go.sum ./
RUN go mod download
COPY nest-rtsp-go/main.go .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o nest-rtsp .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /build/nest-rtsp /usr/local/bin/nest-rtsp

VOLUME /data
EXPOSE 8554

CMD ["nest-rtsp", "-config", "/data/config.yaml"]
