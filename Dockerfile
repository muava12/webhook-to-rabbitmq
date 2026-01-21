# Stage 1: Build the Go application
FROM golang:1.24.5-alpine AS builder

# Set working directory
WORKDIR /app

# Copy go.mod and go.sum
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the source code
COPY main.go ./

# Build the application
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o webhook-service main.go

# Stage 2: Create a minimal runtime image
FROM alpine:latest

# Install ca-certificates for HTTPS requests and tzdata for timezone
RUN apk --no-cache add ca-certificates tzdata

# Set timezone to Asia/Jakarta (WIB)
ENV TZ=Asia/Jakarta

# Set working directory
WORKDIR /root/

# Copy the compiled binary from the builder stage
COPY --from=builder /app/webhook-service .

# Expose the port the service runs on
EXPOSE 8001

# Command to run the application
CMD ["./webhook-service"]