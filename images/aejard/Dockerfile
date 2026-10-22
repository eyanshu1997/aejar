FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/ ./cmd/aejard ./cmd/aejar ./cmd/aejar-discord

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/aejard /out/aejar /out/aejar-discord /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/aejard"]
