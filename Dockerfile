# Stage 1: Build
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy dependency files and download
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o weclawbot-api .

# Stage 2: Runtime
FROM alpine:latest

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Copy binary from builder
COPY --from=builder /app/weclawbot-api .

# Create config directory and volume
RUN mkdir -p /app/config && \
    ln -s /app/weclawbot-api /usr/local/bin/weclawbot-api && \
    ln -s /app/weclawbot-api /usr/local/bin/bot

# Expose default API port
EXPOSE 26322

# Run
ENTRYPOINT ["/app/weclawbot-api"]
