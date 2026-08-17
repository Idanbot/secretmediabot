# syntax=docker/dockerfile:1.12

FROM golang:1.26.6-alpine3.24 AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -buildvcs=false \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/secretmediabot \
      ./cmd/bot

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/secretmediabot /app/secretmediabot

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/app/secretmediabot"]
