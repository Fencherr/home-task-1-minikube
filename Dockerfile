FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY app/go.mod app/go.sum ./
RUN go mod download
COPY app/ .
RUN CGO_ENABLED=0 GOOS=linux go build -o /qoves-api .

FROM alpine:3.19
RUN apk --no-cache add ca-certificates
COPY --from=builder /qoves-api /qoves-api
EXPOSE 8080
ENTRYPOINT ["/qoves-api"]