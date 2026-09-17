# syntax=docker/dockerfile:1

# Build stage: compiles a static strata-server binary. CGO_ENABLED=0 is safe
# here because nothing in this module uses cgo (verified: no `import "C"`
# anywhere in the tree), so the binary needs no libc at runtime and the final
# stage can be distroless rather than needing a full base image.
FROM golang:1.26 AS build

WORKDIR /src

# Dependencies first so `go build` below only re-runs when go.mod/go.sum
# actually change, not on every source edit.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" \
    -o /out/strata-server ./cmd/strata-server

# Final stage: distroless static, nonroot. No shell, no package manager, no
# redis-cli -- this image is the server and nothing else. redis-cli or
# strata-cli against it run from the host or a separate container.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/strata-server /usr/local/bin/strata-server

# Not baked into the image: a data directory here would be silently
# discarded on `docker rm`, and this is exactly the trap plan.md's T9.3
# calls out -- "someone will try it, lose their data, and conclude your
# database is broken." /data must be a mounted volume at run time, e.g.:
#   docker run -v strata-data:/data -p 6380:6380 strata-server
VOLUME ["/data"]
EXPOSE 6380

ENTRYPOINT ["/usr/local/bin/strata-server"]
CMD ["-addr", ":6380", "-data-dir", "/data"]
