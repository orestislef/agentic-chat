# One image holds everything: the hub, the web UI, and the client for every
# platform, which the hub serves itself so a setup depends on nothing else.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
ENV CGO_ENABLED=0
RUN set -e; \
    flags="-s -w -X github.com/orestislef/agentic-chat/internal/version.Version=${VERSION}"; \
    mkdir -p /out/downloads /out/data; \
    go build -trimpath -ldflags "$flags" -o /out/agentic-chat ./cmd/agentic-chat; \
    for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
      os="${target%/*}"; arch="${target#*/}"; ext=""; \
      [ "$os" = "windows" ] && ext=".exe"; \
      GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$flags" \
        -o "/out/downloads/agentic-chat-$os-$arch$ext" ./cmd/agentic-chat; \
    done

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agentic-chat /usr/local/bin/agentic-chat
COPY --from=build /out/downloads /downloads
COPY --from=build --chown=65532:65532 /out/data /data

ENV AC_ADDR=:8080 AC_DATA_DIR=/data AC_DOWNLOADS_DIR=/downloads
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/agentic-chat", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/agentic-chat"]
CMD ["serve"]
