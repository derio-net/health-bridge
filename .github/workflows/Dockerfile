# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /health-bridge .

# Runtime stage
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /health-bridge /health-bridge

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/health-bridge"]
