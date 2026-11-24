# syntax=docker/dockerfile:1
# Control Plane image: web UI built and embedded into one static Go binary on a distroless, non-root base.

FROM node:24.21.0-alpine@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist/app ./web/dist/app
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ai-platform ./cmd/ai-platform \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/ai-platform /usr/local/bin/ai-platform
# Owned by the nonroot user so a fresh named volume inherits writable, private permissions.
COPY --from=build --chown=65532:65532 --chmod=0700 /out/data /data
USER 65532:65532
ENV KINDEV_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["ai-platform", "health"]
# Listens on all interfaces inside the container; compose.yml publishes it on the host's loopback only.
ENTRYPOINT ["ai-platform"]
CMD ["serve", "--addr", "0.0.0.0:8080", "--data-dir", "/data"]
