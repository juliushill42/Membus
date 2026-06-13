# syntax=docker/dockerfile:1.6
# ====================================================================
# Stage 1 — Zig WASM Vector Math Core
# ====================================================================
FROM alpine:3.19 AS zig-builder
RUN apk add --no-cache curl tar xz

WORKDIR /zig
# FIX [C6]: explicit arch detection with fatal fallback
RUN set -e; \
    ARCH=$(uname -m); \
    case "$ARCH" in \
      x86_64)          ZIG_ARCH="x86_64" ;; \
      aarch64|arm64)   ZIG_ARCH="aarch64" ;; \
      *) echo "[FATAL] Unsupported architecture: $ARCH" >&2 && exit 1 ;; \
    esac; \
    curl -fsSL "https://ziglang.org/download/0.13.0/zig-linux-${ZIG_ARCH}-0.13.0.tar.xz" \
      | tar -xJ --strip-components=1

WORKDIR /app
COPY math.zig .
# FIX [C2]: -femit-bin=math.wasm ensures output is math.wasm not libmath.wasm
RUN /zig/zig build-lib math.zig \
      -target wasm32-freestanding \
      -dynamic \
      -rdynamic \
      -O ReleaseFast \
      -femit-bin=math.wasm

# Verify the artifact exists and report its size
RUN ls -lh math.wasm && echo "[*] Zig WASM core compiled successfully"

# ====================================================================
# Stage 2 — Protobuf Code Generation
# FIX [C11]: Use official protoc release binary, not Alpine apk version
# ====================================================================
FROM golang:1.22-alpine AS proto-builder
RUN apk add --no-cache curl unzip git

# Pin protoc to 27.0 — Alpine apk ships older versions with proto3 gaps
RUN set -e; \
    ARCH=$(uname -m); \
    case "$ARCH" in \
      x86_64)        PROTOC_ARCH="linux-x86_64" ;; \
      aarch64|arm64) PROTOC_ARCH="linux-aarch_64" ;; \
      *) echo "[FATAL] Unsupported arch for protoc: $ARCH" >&2 && exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/protocolbuffers/protobuf/releases/download/v27.0/protoc-27.0-${PROTOC_ARCH}.zip" \
      -o /tmp/protoc.zip; \
    unzip -q /tmp/protoc.zip -d /usr/local; \
    chmod +x /usr/local/bin/protoc; \
    rm /tmp/protoc.zip

RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.1
RUN go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.3.0

WORKDIR /app
COPY go.mod .
COPY membus.proto .
RUN mkdir -p v1 && \
    protoc \
      --go_out=. \
      --go_opt=paths=source_relative \
      --go-grpc_out=. \
      --go-grpc_opt=paths=source_relative \
      --proto_path=. \
      membus.proto && \
    mv membus/v1/*.go v1/ 2>/dev/null || true; \
    # Handle protoc output path variance
    find . -name "*.pb.go" | head -5

# ====================================================================
# Stage 3 — Go Engine Compilation
# FIX [C1]: go mod download BEFORE source COPY, with BuildKit cache mount
# ====================================================================
FROM golang:1.22-alpine AS go-builder
RUN apk add --no-cache git ca-certificates

WORKDIR /app
# COPY go.mod + go.sum first for layer-cached dependency download
COPY go.mod .

# Download all dependencies before any source is present.
# BuildKit --mount=type=cache avoids re-downloading on source changes.
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

# Copy generated proto files from proto-builder
COPY --from=proto-builder /app/v1 ./v1/

# Copy WASM binary (needed for embedding verification, not runtime embed here)
COPY --from=zig-builder /app/math.wasm .

# Copy application source last (most cache-busted layer)
COPY main.go .

RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod tidy && \
    CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-s -w -X main.BuildTime=$(date -u +%Y%m%dT%H%M%SZ)" \
             -trimpath \
             -o membus \
             main.go

RUN ls -lh membus && echo "[*] Go engine compiled"

# ====================================================================
# Stage 4 — Minimal Production Runtime (distroless-equivalent via alpine)
# ====================================================================
FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S membus && adduser -S membus -G membus

WORKDIR /app

COPY --from=go-builder  /app/membus    .
COPY --from=zig-builder /app/math.wasm .

# Non-root execution
RUN chown -R membus:membus /app
USER membus

VOLUME ["/data"]
ENV DATA_DIR=/data \
    GOMAXPROCS=0

EXPOSE 50051 8080

HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["./membus"]
