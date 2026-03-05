FROM golang:1.24-alpine AS build

WORKDIR /src/nano-cloud
COPY nano-cloud/go.mod nano-cloud/go.sum ./
COPY nano-agent /src/nano-agent
RUN go mod edit -replace github.com/nano-harness/nano-agent=/src/nano-agent
RUN go mod download
COPY nano-cloud/cmd/net-policy-proxy ./cmd/net-policy-proxy
RUN CGO_ENABLED=0 go build -o /out/net-policy-proxy ./cmd/net-policy-proxy

FROM alpine:3.20

RUN apk add --no-cache ca-certificates
COPY --from=build /out/net-policy-proxy /usr/local/bin/net-policy-proxy
ENTRYPOINT ["net-policy-proxy"]
