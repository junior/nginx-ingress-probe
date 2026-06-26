# syntax=docker/dockerfile:1

# --- build a static binary ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.appVersion=${VERSION} -X main.buildTime=${BUILD_TIME}" \
    -o /probe .

# --- minimal, non-root runtime ---
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="nginx-ingress-probe" \
      org.opencontainers.image.description="Diagnostics page to verify the NGINX Plus Ingress Controller after an upgrade" \
      org.opencontainers.image.authors="Adao Oliveira Jr" \
      org.opencontainers.image.url="https://adao.dev" \
      org.opencontainers.image.source="https://github.com/junior/nginx-ingress-probe" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /probe /probe
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/probe"]
