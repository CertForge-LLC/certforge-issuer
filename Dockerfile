FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /certforge-issuer ./cmd/controller

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /certforge-issuer /certforge-issuer
USER 65532:65532
ENTRYPOINT ["/certforge-issuer"]
