FROM golang:1.24-alpine AS build
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src/nano-cloud
COPY nano-cloud/go.mod nano-cloud/go.sum ./
RUN apk add --no-cache git openssh-client && \
    git config --global url."ssh://git@github.com/".insteadOf "https://github.com/"
RUN --mount=type=ssh go mod download

COPY nano-cloud ./
RUN CGO_ENABLED=0 go build -o /out/runtime-nano-agent ./cmd/runtime-nano-agent

FROM alpine:3.20

RUN apk add --no-cache ca-certificates git bash
COPY --from=build /out/runtime-nano-agent /usr/local/bin/runtime-nano-agent

# Create config directory so bind-mount targets have a parent directory available
RUN mkdir -p /root/.config/nano

WORKDIR /workspace
ENTRYPOINT ["runtime-nano-agent"]
