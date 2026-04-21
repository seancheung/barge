# syntax=docker/dockerfile:1.6

FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY internal ./internal

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/barge .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/barge /usr/local/bin/barge
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/barge"]
