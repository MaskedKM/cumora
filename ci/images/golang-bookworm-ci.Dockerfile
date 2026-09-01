# checks job 本地烘焙镜像(runner-maintenance.yml 的 bake job 维护)——
# pr.yml 的 checks job 以此为容器。相比官方 golang:1.24-bookworm 多三层:
#   1. 底换 node:24-bookworm:checks job 的 node 工具链与官方镜像同源
#      (setup-node 仍照跑,互不冲突)。
#   2. Go 1.24 整目录拷自官方 golang:1.24-bookworm——go 是自包含单目录,
#      同 libc 跨镜像拷贝零依赖(集成 SUT 构建/cmd/migrate 全靠它)。
#   3. chromium 系统依赖:npx playwright@1.62.1 install-deps chromium 的
#      apt 包集(版本与仓内 package-lock.json 钉版对齐;playwright 升版
#      若新增依赖,e2e 缺 .so 报错 = 重烘信号)。#292 审计前每轮 apt
#      96.1MB + 索引 9.4MB 走代理,容器层随 job 生灭。
# apt 过代理的抖动配方沿用(禁 pipelining + 重试)写进镜像 apt.conf,
# install-deps 内部的 apt-get 同样吃到。
FROM node:24-bookworm
COPY --from=golang:1.24-bookworm /usr/local/go /usr/local/go
ENV GOPATH=/go
ENV PATH=/usr/local/go/bin:$PATH
RUN mkdir -p /go \
 && printf 'Acquire::Retries "5"; Acquire::http::Pipeline-Depth "0"; Acquire::https::Pipeline-Depth "0";\n' \
      > /etc/apt/apt.conf.d/80flaky-proxy \
 && npx --yes playwright@1.62.1 install-deps chromium \
 && rm -rf /var/lib/apt/lists/* /root/.npm
