# Multi-stage build for one static service binary, selected by --build-arg CMD (ingest | admin).
# Both are CGO-free statics on scratch (no shell, no packages). They connect to the DB over the
# internal docker network (plain TCP); TLS terminates at the proxy. The published port is declared
# by each compose service (ingest :8080, admin :8081), not here.
ARG CMD=ingest
FROM golang:1.25-alpine AS build
ARG CMD
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

FROM scratch
COPY --from=build /out/app /app
USER 65534:65534
ENTRYPOINT ["/app"]
