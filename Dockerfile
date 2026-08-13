# Multi-stage: build a static (CGO-free) binary, ship it on distroless.
# Pure-Go sqlite (modernc) means CGO_ENABLED=0 works and the final image needs
# no libc. distroless/static provides CA certs for HTTPS (Homebox/OpenRouter).
# Build stage runs on the build host's arch and cross-compiles to TARGETARCH —
# no QEMU-emulated compiler for the arm64 image.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/docfetch ./cmd/docfetch

FROM gcr.io/distroless/static-debian12:latest
COPY --from=build /out/docfetch /docfetch
VOLUME ["/data"]
# Default to serve (scheduler + portal, one process); override for `once`/`probe`.
ENTRYPOINT ["/docfetch"]
CMD ["serve", "--config", "/config/config.yaml"]
