FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY internal ./internal
COPY web ./web

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /seedlink-auditor .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates \
    && mkdir -p /config /data

ENV SEEDLINK_CONFIG_PATH=/config/settings.json \
    PORT=8090

COPY --from=builder /seedlink-auditor /seedlink-auditor

EXPOSE 8090

ENTRYPOINT ["/seedlink-auditor"]
