FROM golang:1.24-alpine AS build

WORKDIR /src/nano-cloud
COPY nano-cloud/go.mod nano-cloud/go.sum ./
COPY nano-agent /src/nano-agent
RUN go mod edit -replace github.com/nano-harness/nano-agent=/src/nano-agent
RUN go mod download
COPY nano-cloud ./
RUN CGO_ENABLED=0 go build -o /out/runtime-nano-agent ./cmd/runtime-nano-agent

FROM alpine:3.20

RUN apk add --no-cache ca-certificates git bash
COPY --from=build /out/runtime-nano-agent /usr/local/bin/runtime-nano-agent

# Bake in the default configuration
COPY nano-cloud/docker/nano-agent-runtime/config.example.yaml /root/.config/nano/config.yaml

# Ensure configuration directory permissions
RUN chmod 644 /root/.config/nano/config.yaml

WORKDIR /workspace
ENTRYPOINT ["runtime-nano-agent"]
