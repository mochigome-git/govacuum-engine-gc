# Stage 1: Build the Go program
FROM golang:1.25.7-alpine AS builder

# Accept platform args from buildx
ARG TARGETOS
ARG TARGETARCH

# Configure Go environment for static build
ENV CGO_ENABLED=0 \
    GOOS=$TARGETOS \
    GOARCH=$TARGETARCH

WORKDIR /app

# Cache dependencies first
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary (adjust the main package path if needed)
RUN go build -o main ./cmd/

# --- Stage 2: Runtime ---
FROM gcr.io/distroless/base-debian11

# Copy built binary
COPY --from=builder /app/main /app/main

# Working directory and port
WORKDIR /app
EXPOSE 8080

# Run the app
ENTRYPOINT ["/app/main"]


# Build Image with command
# docker buildx create --use
# docker buildx build \
#   --platform linux/amd64,linux/arm64 \
#   -t mochigome/patch:2.21v.ecs \
#   --push .


# legacy build
# docker build --no-cache -t patch:2.21v.ecs .
# docker tag patch:2.21v.ecs mochigome/patch:2.21v.ecs
# docker push mochigome/patch:2.21v.ecs

# AWS ECR
# docker tag patch:2.21v.ecs 590183751536.dkr.ecr.ap-southeast-1.amazonaws.com/patch:2.21v.ecs
# docker push 590183751536.dkr.ecr.ap-southeast-1.amazonaws.com/patch:2.21v.ecs

# current version : 2.21v.ecs