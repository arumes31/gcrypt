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

# Run as a non-root user. Base alpine has no shadow `useradd`, so use BusyBox
# `adduser`. The app must live under the user's home (not /root, which is mode
# 0700 and unusable by a non-root user).
RUN adduser -D -h /home/gcrypt gcrypt

WORKDIR /home/gcrypt

# Copy the binary from the builder
COPY --from=builder /app/gcrypt .

# Create the sync folder and hand it plus the binary/home to the non-root user.
# (config.Save creates /home/gcrypt/.config/gcrypt itself at runtime.)
RUN mkdir -p /data/sync \
    && chown -R gcrypt:gcrypt /data/sync /home/gcrypt

# Set environment variables (config lives under the non-root user's home)
ENV GCRYPT_CONFIG_PATH=/home/gcrypt/.config/gcrypt/config.yaml

# Switch to the non-root user for execution
USER gcrypt

# Command to run
ENTRYPOINT ["./gcrypt"]
