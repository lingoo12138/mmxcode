# 构建
FROM golang:1.26-alpine AS builder

WORKDIR /src

# 受限网络下构建时要给出出口：docker build --build-arg GOPROXY=...
# 默认值就是 go 自己的默认值，不传也不影响。
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

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

# 默认配置打进镜像：没有 bind mount 时镜像自己也能跑。
# 想改配置就挂载自己的文件盖住它。
COPY config.docker.yaml /app/config.yaml

USER mmxcode
WORKDIR /app

ENV MMXCODE_CONFIG=/app/config.yaml
ENV TZ=Asia/Shanghai

EXPOSE 8080
VOLUME ["/data"]

# 健康检查：网关有一个不用 key 的 /healthz。
#
# 它不用 key 是故意的 —— 密钥不进镜像，健康检查手里也就没有 key；它问的
# 是「进程还在吗」，不是「这个人是谁」。alpine 自带 busybox 的 wget，
# 镜像里不用再多装东西。
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

# 停的时候给网关一点时间：它关之前要让在飞的请求和后台的活儿收尾
# （最多 10 秒）。
STOPSIGNAL SIGTERM

ENTRYPOINT ["/usr/local/bin/mmxcode"]
CMD ["serve"]
