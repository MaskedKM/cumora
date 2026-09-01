# go job 本地烘焙镜像(runner-maintenance.yml 的 bake job 维护)——
# pr.yml 的 go job 以此为容器。相比官方 golang:1.24-alpine 多装:
#   - gcc + musl-dev:apps/stack 的 -race 需要 cgo(#290)。#292 审计前
#     每轮 `apk add --no-cache gcc musl-dev` 走代理 ~60-70MB/轮,且失败
#     分支会静默降级掉 race——烘进镜像后该分支已从 pr.yml 删除。
#   - curl:tier-2a 源码兜底的 apk 引导腿永久短路(alpine 默认不带)。
# 烘焙幂等(层缓存命中=秒级),nightly 顺带 best-effort 刷新基础镜像。
# docker build 内无 job env,代理经 build-arg 传入(bake job 负责)。
FROM golang:1.24-alpine
RUN apk add --no-cache gcc musl-dev curl
