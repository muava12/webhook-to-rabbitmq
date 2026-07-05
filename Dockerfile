# Stage 0: Build the React frontend
FROM node:26-alpine AS frontend
WORKDIR /app
COPY frontend/package.json ./
RUN npm install
COPY frontend/ ./
RUN OUT_DIR=/output npm run build

# Stage 1: Build the Go application
FROM golang:1.24.5-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY --from=frontend /output/ ./static/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o webhook-service .

# Stage 2: Runtime
FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
ENV TZ=Asia/Jakarta
WORKDIR /root/
COPY --from=builder /app/webhook-service .
EXPOSE 8001
CMD ["./webhook-service"]
