ARG BASE_GOLANG_IMAGE=docker.io/library/golang:1.24.4
ARG BASE_LINUX_IMAGE=docker.io/library/golang:1.24.4
ARG GOPROXY=https://goproxy.cn,direct

FROM ${BASE_GOLANG_IMAGE} AS builder
ARG GOPROXY
ENV GOPROXY=${GOPROXY}
WORKDIR /app
COPY . .
RUN go build -o gateway ./cmd/gateway

FROM ${BASE_LINUX_IMAGE}
WORKDIR /app
COPY --from=builder /app/gateway .
ENTRYPOINT ["/app/gateway"]
