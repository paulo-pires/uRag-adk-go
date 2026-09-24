# Base 1.26 do lado local (o go.mod exige `go 1.26.0`, que a 1.25-alpine nao
# compila) com o WORKDIR do lado do master: `replace urag-stack/pkg/glitchtip =>
# ../pkg/glitchtip` so resolve para /work/pkg/glitchtip -- destino do
# COPY --from=glitchtip -- se o WORKDIR for /work/uRag-adk-go.
FROM golang:1.26-alpine AS build
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
