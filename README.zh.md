# barge

[English](README.md) | **简体中文**

`barge` 是一个命令行工具，从任意 container registry（Docker Hub、GHCR、GCR、Quay、私有 registry 等）拉取镜像并打包为 `docker load` 兼容的 tar 文件。**不依赖 Docker daemon**。

典型场景：在受限网络中把镜像"搬运"到内网，或归档某个版本的镜像到磁盘。

## 特性

- 跨平台：macOS / Windows / Linux，amd64 / arm64，单个静态二进制
- 代理支持：`--proxy` 参数或读取 `HTTPS_PROXY` 环境变量
- 断点续传：HTTP Range 请求 + sha256 校验，中断后重跑自动接着下载
- 自动重试：网络错误 / 5xx / EOF 时按指数退避重试（可配置次数）
- 层复用：content-addressable 缓存，拉多个镜像时相同 layer 自动复用
- 并发下载：多个 layer 同时拉
- 认证支持：`--username/--password`、`--password-stdin`，自动读取 `~/.docker/config.json`
- 多架构镜像：通过 `--platform` 指定（默认 `linux/<当前主机架构>`）
- 零第三方依赖：仅用 Go 标准库

## 安装

### 下载预编译二进制

从 [Releases](../../releases) 页下载对应平台的压缩包：

- `barge_vX.Y.Z_darwin_amd64.tar.gz` / `darwin_arm64.tar.gz`
- `barge_vX.Y.Z_windows_amd64.zip` / `windows_arm64.zip`
- `barge_vX.Y.Z_linux_amd64.tar.gz` / `linux_arm64.tar.gz`

解压后将可执行文件放到 `PATH` 中即可。

### 从源码构建（需要 Docker 或 Go）

用 Docker 交叉编译：

```bash
docker build -t barge .
docker run --rm -v "$PWD:/data" barge pull alpine:3.20
```

或直接用 Go：

```bash
go build -trimpath -ldflags "-s -w" -o barge .
```

## 使用

### 基本用法

```bash
barge pull nginx:1.25
# => 生成 nginx_1.25.tar

docker load -i nginx_1.25.tar
```

### 指定输出路径与平台

```bash
barge pull -o /tmp/img.tar -p linux/arm64 alpine:3.20
```

### 使用代理

```bash
barge pull -x http://127.0.0.1:7890 nginx:latest
# -x 是 --proxy 的简写

# 或通过环境变量
export HTTPS_PROXY=http://127.0.0.1:7890
barge pull nginx:latest
```

### 私有镜像（GHCR / 私有 registry）

**方式 A：传入凭据**

```bash
# 从 stdin 读取（推荐，不会进 shell 历史）
echo $GITHUB_TOKEN | barge pull --password-stdin -u YOUR_USERNAME \
    ghcr.io/org/private-image:tag

# 或直接传参（不推荐）
barge pull -u YOUR_USERNAME --password $GITHUB_TOKEN \
    ghcr.io/org/private-image:tag
```

**方式 B：复用 docker login 凭据**

如果已经 `docker login ghcr.io` 过，`barge` 会自动读取 `~/.docker/config.json`：

```bash
docker login ghcr.io  # 一次性登录
barge pull ghcr.io/org/private-image:tag  # 自动认证
```

**方式 C：指定 docker config 路径**

```bash
barge pull --docker-config /path/to/config.json ghcr.io/org/image:tag
```

### 通过 digest 拉取

```bash
barge pull alpine@sha256:c5b1261d6d3e43071626931fc004f10189d3b5df5e3b85df68e6b0dd1b1a8f5e
```

### 断点续传与缓存

下载中断（Ctrl+C、网络抖动）后，**再跑同样的命令**即可 — 已下载的 blob 会自动复用，未完成的会从断点继续：

```bash
barge pull very-large-image:tag
# ...中断...
barge pull very-large-image:tag  # 自动续传
```

所有 blob 以 content-addressable 方式扁平缓存在 `~/.barge/blobs/` 下，按 sha256 命名。同一 registry 不同镜像共享的 layer 只下载一次；未完成的 blob 会带 `.part` 后缀。

缓存位置可通过环境变量 `BARGE_HOME` 整体重定向：

```bash
export BARGE_HOME=/mnt/bigdisk/barge
barge pull nginx:1.25
# => blob 存到 /mnt/bigdisk/barge/blobs/
```

### 查看与清理缓存

```bash
barge status              # 显示 blob 数量、.part 数量、总占用
barge clean               # 只清理 .part 临时文件
barge clean --all         # 同时清除所有 blob 缓存
barge clean -a            # 同上
```

## 命令与参数

### 顶层命令

| 命令 | 说明 |
|---|---|
| `barge pull <image> [flags]` | 拉取镜像并打包为 tar |
| `barge status` | 查看缓存状态 |
| `barge clean [--all]` | 清理临时文件（`--all` 同时清 blob 缓存） |
| `barge --version` | 打印版本 |

### 拉取参数

| 参数 | 简写 | 说明 |
|---|---|---|
| `--output` | `-o` | 输出 tar 文件路径（默认 `<repo>_<tag>.tar`） |
| `--platform` | `-p` | 目标平台 `os/arch[/variant]`（默认 `linux/<当前主机架构>`） |
| `--proxy` | `-x` | HTTP/HTTPS 代理 URL（默认读 `HTTPS_PROXY`） |
| `--concurrency` | `-c` | 并发下载 layer 数（默认 3） |
| `--retries` | `-r` | 每个 blob/manifest 的最大重试次数（默认 3，遇网络错误/5xx/EOF 自动重试，指数退避 1s → 2s → 4s…） |
| `--username` | `-u` | registry 用户名 |
| `--password` | | 密码/token（不推荐，建议用 `--password-stdin`） |
| `--password-stdin` | | 从 stdin 读取密码/token |
| `--docker-config` | | docker config.json 路径 |

### 环境变量

| 变量 | 说明 |
|---|---|
| `BARGE_HOME` | 数据根目录（默认 `~/.barge`）。blob 缓存放在 `$BARGE_HOME/blobs/` |
| `HTTPS_PROXY` / `HTTP_PROXY` | 代理（`--proxy` 未指定时生效） |
| `DOCKER_CONFIG` | Docker 配置目录（`--docker-config` 未指定时用 `$DOCKER_CONFIG/config.json`） |

## 镜像引用格式

```
nginx                           -> registry-1.docker.io/library/nginx:latest
nginx:1.25                      -> registry-1.docker.io/library/nginx:1.25
alice/app:1.0                   -> registry-1.docker.io/alice/app:1.0
ghcr.io/owner/repo:tag          -> ghcr.io/owner/repo:tag
gcr.io/project/img@sha256:abc   -> gcr.io/project/img@sha256:abc
localhost:5000/team/svc:dev     -> localhost:5000/team/svc:dev
```

## 工作原理

1. 解析镜像引用，定位 registry / 仓库 / tag 或 digest
2. 向 `GET /v2/` 发起请求；若收到 `401`，按 `Www-Authenticate` 头协商 Bearer token 或 Basic auth
3. 拉 manifest；若是 manifest list / OCI index，按 `--platform` 选择子 manifest 并重新拉
4. 对 config 和各 layer 发 `GET /v2/<repo>/blobs/<digest>`，支持 `Range` 断点续传，sha256 校验后原子 rename
5. 打包成 `manifest.json + <config>.json + <layer>.tar.gz` 的 tar（`docker load` 兼容格式）

## 构建

项目零第三方依赖，构建只需 Go 1.22+：

```bash
go build -trimpath -ldflags "-s -w -X main.version=dev" -o barge .
```

交叉编译示例（在任意平台产出 Windows arm64 二进制）：

```bash
GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build -o barge.exe .
```

## 发版

推送 `v*` 标签会触发 GitHub Actions workflow，自动产出 6 个平台的二进制（macOS / Windows / Linux × amd64 / arm64）并发布到 GitHub Release，附带 `checksums.txt`。

```bash
git tag v0.1.0
git push origin v0.1.0
```

## 许可

MIT
