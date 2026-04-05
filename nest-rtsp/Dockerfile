FROM golang:1.25rc1-bookworm AS builder
ENV GOTOOLCHAIN=auto

WORKDIR /build
COPY nest-rtsp-go/go.mod nest-rtsp-go/go.sum ./
RUN go mod download
COPY nest-rtsp-go/main.go .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o nest-rtsp .

FROM alpine:3.21

RUN apk add --no-cache bash jq

COPY --from=builder /build/nest-rtsp /usr/local/bin/nest-rtsp
COPY run.sh /run.sh
RUN chmod +x /run.sh

CMD ["/run.sh"]
