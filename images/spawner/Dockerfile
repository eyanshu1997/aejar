FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/aejar-spawner ./cmd/aejar-spawner

# The docker CLI only; the daemon is the host's (rootless) one via its socket.
FROM docker:29-cli
COPY --from=build /out/aejar-spawner /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/aejar-spawner"]
