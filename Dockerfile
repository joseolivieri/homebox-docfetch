# Multi-stage: build a static (CGO-free) binary, ship it on distroless.
# Pure-Go sqlite (modernc) means CGO_ENABLED=0 works and the final image needs
# no libc. distroless/static provides CA certs for HTTPS (Homebox/OpenRouter).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/docfetch ./cmd/docfetch

FROM gcr.io/distroless/static-debian12:latest
COPY --from=build /out/docfetch /docfetch
VOLUME ["/data"]
# Default to the long-running scheduler; override for `once`/`probe`.
ENTRYPOINT ["/docfetch"]
CMD ["scheduler", "--config", "/config/config.yaml"]
