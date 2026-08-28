# hostshift — static binary in a distroless image (PLAN §5.7).
#
# Nothing is needed at runtime but the binary: no shell, no package manager, no
# libc. hostshift speaks plain HTTP to one upstream on the project's compose
# network and never terminates TLS itself.

FROM golang:1.26 AS build
WORKDIR /src

# Dependencies first, so a source-only change does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /hostshift ./cmd/hostshift

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /hostshift /hostshift

# Bind 0.0.0.0 inside the container: 127.0.0.1 is unreachable from the DDEV
# router. The port is exposed to the compose network only — never published to
# the host (PLAN §5.7).
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/hostshift"]
# -C /project so the image's own default works. Without it a bare `docker run`
# resolves the map from the container's working directory, finds no config and
# exits 2 — the compose service overrides `command` and so never noticed.
CMD ["proxy", "-C", "/project", "--listen", "0.0.0.0:8080", "--upstream", "http://web:80"]
