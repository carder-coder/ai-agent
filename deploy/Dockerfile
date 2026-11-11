FROM golang:1.25-alpine AS build

RUN apk add --no-cache ca-certificates
RUN mkdir -p /tmp/traces && chown -R 65532:65532 /tmp/traces
WORKDIR /src/gateway
COPY gateway/go.mod ./
COPY gateway/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /gateway ./cmd/gateway

FROM scratch
COPY --from=build /gateway /gateway
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /tmp/traces /tmp/traces
WORKDIR /tmp
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/gateway"]
