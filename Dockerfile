# Multi-stage build for gcrypt
FROM golang:1.22-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git gcc musl-dev

WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application.
# CGO is disabled (CGO_ENABLED=0) deliberately: the SQLite layer uses the
# pure-Go driver modernc.org/sqlite (see internal/drive/store.go), so the
# database needs no C toolchain or libc linkage and the binary stays fully
# static. (No mattn/go-sqlite3 / CGO dependency is imported anywhere.)
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o gcrypt ./cmd/gcrypt/main.go

# Final stage
FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /root/

# Copy the binary from the builder
COPY --from=builder /app/gcrypt .

# Create a directory for the sync folder
RUN mkdir -p /data/sync

# Set environment variables
ENV GCRYPT_CONFIG_PATH=/root/.config/gcrypt/config.yaml

# Command to run
ENTRYPOINT ["./gcrypt"]
