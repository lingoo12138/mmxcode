# 构建
FROM golang:1.26-alpine AS builder

WORKDIR /src

# 先只拷依赖清单，依赖没变这一层就不用重建
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 静态编译，镜像里不需要 libc
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mmxcode ./cmd/mmxcode

# 运行
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 mmxcode \
    && mkdir -p /data \
    && chown mmxcode:mmxcode /data

COPY --from=builder /out/mmxcode /usr/local/bin/mmxcode

USER mmxcode
WORKDIR /app

ENV MMXCODE_CONFIG=/app/config.yaml
ENV TZ=Asia/Shanghai

EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/usr/local/bin/mmxcode"]
CMD ["serve"]
