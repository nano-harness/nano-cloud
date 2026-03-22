FROM golang:1.24-alpine AS build
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src/nano-cloud
COPY nano-cloud/go.mod nano-cloud/go.sum ./
COPY nano-agent /src/nano-agent
COPY nano-cloud/docker/common/setup-go-mod.sh /tmp/setup-go-mod.sh
RUN chmod +x /tmp/setup-go-mod.sh && /tmp/setup-go-mod.sh
COPY nano-cloud ./
RUN CGO_ENABLED=0 go build -o /out/runtime-cli ./cmd/runtime-cli
RUN CGO_ENABLED=0 go build -o /out/runtime-cli-wrapper ./cmd/runtime-cli-wrapper

FROM alpine:3.20

RUN apk add --no-cache ca-certificates bash
COPY --from=build /out/runtime-cli /usr/local/bin/runtime-cli
COPY --from=build /out/runtime-cli-wrapper /usr/local/bin/runtime-cli-wrapper
WORKDIR /workspace
ENTRYPOINT ["runtime-cli-wrapper"]
