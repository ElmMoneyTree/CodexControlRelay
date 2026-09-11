FROM golang:1.26.5-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
    -o /out/codex-relay ./cmd/codex-relay && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/codex-relay /usr/local/bin/codex-relay
COPY --chown=65532:65532 --from=build /out/data /var/lib/codex-relay
EXPOSE 8787
VOLUME ["/var/lib/codex-relay"]
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/codex-relay"]
CMD ["--config", "/etc/codex-relay/relay.yaml"]
