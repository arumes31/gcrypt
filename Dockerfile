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

# Build the application
# We use CGO_ENABLED=0 for a static binary if possible, 
# but systray/sqlite might need CGO on some platforms.
# For a headless Linux container, we might skip the tray.
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
