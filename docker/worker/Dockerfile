ARG BASE_GOLANG_IMAGE=docker.io/library/golang:1.24.4
ARG BASE_LINUX_IMAGE=docker.io/library/golang:1.24.4
ARG GOPROXY=https://goproxy.cn,direct
ARG DOCKER_CLI_VERSION=28.5.1
ARG DOCKER_CLI_IMAGE=docker.m.daocloud.io/library/docker:${DOCKER_CLI_VERSION}-cli

FROM ${BASE_GOLANG_IMAGE} AS builder
ARG GOPROXY
ENV GOPROXY=${GOPROXY}
WORKDIR /app
COPY . .
RUN go build -o worker ./cmd/worker

FROM ${DOCKER_CLI_IMAGE} AS docker-cli

FROM ${BASE_LINUX_IMAGE}
WORKDIR /app
COPY --from=builder /app/worker .
COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker
RUN chmod +x /usr/local/bin/docker

ENTRYPOINT ["/app/worker"]
