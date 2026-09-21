FROM golang:1.25-alpine AS build
WORKDIR /work/uRag-adk-go
COPY go.mod go.sum ./
COPY --from=glitchtip . /work/pkg/glitchtip
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/agent ./cmd/agent

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/agent /usr/local/bin/agent
ENTRYPOINT ["agent"]
