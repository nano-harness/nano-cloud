FROM golang:1.24-alpine AS build
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src/nano-cloud
COPY nano-cloud/go.mod nano-cloud/go.sum ./
COPY nano-agent /src/nano-agent
COPY nano-cloud/docker/common/setup-go-mod.sh /tmp/setup-go-mod.sh
RUN chmod +x /tmp/setup-go-mod.sh && /tmp/setup-go-mod.sh
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
