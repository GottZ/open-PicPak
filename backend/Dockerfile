# Multi-stage build for the ingest service. Static binary -> scratch (no shell, no packages).
# Connects to the DB over the internal docker network (plain TCP); TLS terminates at the proxy.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ingest ./cmd/ingest

FROM scratch
COPY --from=build /out/ingest /ingest
EXPOSE 8080
USER 65534:65534
ENTRYPOINT ["/ingest"]
