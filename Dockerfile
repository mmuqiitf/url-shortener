# --- build stage -------------------------------------------------------------
FROM golang:1.25-alpine AS build

# Build tools for cgo are NOT needed (modernc.org/sqlite is pure Go),
# so we keep the image small. set CGO_ENABLED=0 explicitly to ensure
# the resulting binary is statically linked.
ENV CGO_ENABLED=0 GOOS=linux

WORKDIR /src

# Cache module downloads in a separate layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -trimpath removes the build machine's file paths from the binary.
# -ldflags "-s -w" strips symbol/debug info to shrink the binary.
# The /data directory holds the SQLite file; create it here so the
# distroless image inherits the right mode and ownership.
RUN go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && mkdir -p /data

# --- runtime stage -----------------------------------------------------------
# gcr.io/distroless/static-debian12:nonroot is a tiny image that contains
# only ca-certificates, /etc/passwd for the `nonroot` user, and tzdata.
# No shell, no package manager — fewer attack surfaces.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/server /server

# The distroless `nonroot` user (uid 65532) already owns /home/nonroot.
# We use that as WORKDIR so the SQLite file is written somewhere the
# nonroot user can create, and a mounted volume can persist it.
WORKDIR /home/nonroot

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/server"]
