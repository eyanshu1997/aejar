# coherence, built from the submodule without modifying it.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY coherence/ .
RUN CGO_ENABLED=0 go build -o /out/ ./cmd/coherence-server ./cmd/coherence-doc

FROM debian:bookworm-slim
# git: coherence's doc version history.
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 1600 --create-home --home-dir /data coherence
COPY --from=build /out/ /opt/coherence/bin/
COPY coherence/www /opt/coherence/www
ENV COHERENCE_HOME=/opt/coherence HOME=/data COHERENCE_BIND=0.0.0.0 COHERENCE_PORT=8080
USER coherence
EXPOSE 8080
ENTRYPOINT ["/opt/coherence/bin/coherence-server"]
