# docker-control

通用、可迁移的受控 Docker sandbox 平台。任何能够使用 HTTP 和 WebSocket 的程序都能
接入，不依赖 AstrBot、特定机器人框架或特定语言。

```text
任意客户端 ── REST / WebSocket ──> docker-control ──> Docker Engine
                                       │
                                       └─ sandbox-<key>
```

默认容器名为 `sandbox-<key>`，可通过 `CONTAINER_NAME_PREFIX` 修改。网络和资源策略
由部署方固定；持有管理 Bearer Token 的受信客户端可为 sandbox 选择镜像。

Sandbox 使用 Docker 默认的非特权权限集，明确设置 `Privileged=false`，不再移除全部
capabilities 或强制 `no-new-privileges`。这允许 APT、dpkg、编译器和常见工具正常工作，
但不会挂载 Docker Socket 或宿主机目录，也不会以 `--privileged` 模式启动。

## v3.4.0 发布说明

- 沙箱恢复 Docker 默认非特权 capabilities，支持 `apt-get`、`dpkg` 和多语言工具链；
- 始终保持 `Privileged=false`，不提供 Docker Socket 或宿主机目录；
- 默认端口为 `16544`，端口占用时可自动选择后续端口；
- 支持安全切换自定义镜像、readiness 状态、会话重建和实时 WebSocket 终端；
- 单一二进制提供 `server`、`install`、`uninstall`、`status`、`config` 和 `version`。
- 支持容器系统层与 `/workspace` 的持久化还原点，可快速创建、列出、恢复和删除。

从 v3.2.0 或更早版本升级时，替换并重启控制器后，需要重建既有 sandbox 才能应用
新的容器权限配置。新建容器无需额外参数：

```bash
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y ffmpeg python3 nodejs
```

发布二进制与 `SHA256SUMS.txt` 位于 GitHub
[Releases](https://github.com/ycxom/docker-control/releases)。

## 通用 API seam

API 资源使用 `sandbox`，调用方只需提供 8–64 位稳定 `key`：

```text
GET    /v1/health
GET    /v1/capabilities
GET    /v1/sandboxes
PUT    /v1/sandboxes/{key}
GET    /v1/sandboxes/{key}
DELETE /v1/sandboxes/{key}
POST   /v1/sandboxes/{key}/rebuild
GET    /v1/sandboxes/{key}/snapshots
POST   /v1/sandboxes/{key}/snapshots
POST   /v1/sandboxes/{key}/snapshots/{id}/restore
DELETE /v1/sandboxes/{key}/snapshots/{id}
GET/WS /v1/sandboxes/{key}/terminal
PUT    /v1/sandboxes/{key}/files?path=...
GET    /v1/sandboxes/{key}/files?path=...
GET    /openapi.yaml
```

管理接口使用标准 `Authorization: Bearer <token>`。健康、能力和 OpenAPI 文档无需
认证。结构化错误统一包含 `error.code`、`error.message`、`error.status`。

运行后可以直接取得契约：

```bash
curl http://127.0.0.1:16544/openapi.yaml
curl http://127.0.0.1:16544/v1/capabilities
```

OpenAPI 3.1 可用于生成其他语言客户端；WebSocket 扩展通过
`x-websocket-protocol: docker-control-terminal-v1` 描述。
仓库内契约源文件为 [openapi.yaml](internal/controller/openapi.yaml)。

## 最小接入流程

```bash
# 幂等创建或取得 sandbox
curl -X PUT -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:16544/v1/sandboxes/my-client-001

# 查询
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:16544/v1/sandboxes/my-client-001

# 删除
curl -X DELETE -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:16544/v1/sandboxes/my-client-001
```

框架适配器通常只需实现四步：生成稳定 key、`PUT` 确保 sandbox、通过 WS 执行命令、
在会话结束时选择性 `DELETE`。

每个 sandbox 返回 `ready` 和 `readiness`。终端、命令和文件接口只接受
`ready=true`；`starting`、`unhealthy` 或 `stopped` 会立即返回 503。环境发生无法通过
普通命令修复的严重损坏时，调用 `POST .../rebuild` 删除并以相同 key 重建。

`PUT` 和 `POST .../rebuild` 可传入 `{"image":"ubuntu:22.04"}`。镜像与现有容器不同
时，控制器先在后台拉取并返回 503；确认新镜像可用后再次请求才替换旧容器，避免切换
失败导致可用环境提前丢失。

## 还原点与快速恢复

还原点同时捕获容器可写系统层（已安装软件、系统配置）和 `/workspace`。它们以带受管
标签的本地 Docker 镜像持久化，控制器重启后仍可使用；每个 sandbox 最多 10 个。

```bash
# 创建
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"before-ffmpeg-upgrade"}' \
  http://127.0.0.1:16544/v1/sandboxes/my-client-001/snapshots

# 列出
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:16544/v1/sandboxes/my-client-001/snapshots

# 恢复
curl -X POST -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:16544/v1/sandboxes/my-client-001/snapshots/<snapshot-id>/restore
```

恢复会先用快照启动临时容器并等待 readiness，通过后才替换当前容器。删除 sandbox
不会自动删除其还原点，便于重新创建后恢复；`uninstall` 默认会清理所有受管还原点。

## WebSocket 终端

```text
ws://<host>/v1/sandboxes/<key>/terminal
Authorization: Bearer <token>
```

客户端发送：

```json
{"type":"exec","request_id":"1","command":"python -m pip install requests","workdir":"/workspace"}
```

事件顺序：

```text
ready → started → output* → exit
```

`output` 是 Base64 实时字节流。窗口超过 `TERMINAL_IDLE_SECONDS` 不活跃时发送
`reclaimed` 并回收；执行中的命令只受执行超时限制。

## 容器内受控接口

创建 sandbox 时自动注入：

```text
CONTROLLED_DOCKER_ENDPOINT=ws://.../v1/controlled/<key>/terminal
CONTROLLED_DOCKER_HEALTH_ENDPOINT=http://.../v1/controlled/<key>
CONTROLLED_DOCKER_TOKEN=<每个 sandbox 独立令牌>
```

受控令牌只能访问自身 sandbox，不能列举、创建或删除其他资源。

## 快速运行

单一二进制已经融合配置、运行、安装和卸载能力。首次前台启动：

```bash
./dist/docker-control-v3.4.0-linux-amd64 server \
  --port 16544 \
  --port-fallbacks 20 \
  --image ubuntu:22.04 \
  --controlled-endpoint 'ws://host.docker.internal:{port}'
```

首次执行会在当前目录创建：

```text
$PWD/.docker-control/docker-control.env
$PWD/.docker-control/installation.json
$PWD/.docker-control/runtime.json
```

管理 Token 自动生成并仅保存在权限为 `0600` 的配置文件中。端口冲突时自动向后
尝试；`--port 0` 自动选择空闲端口。`{port}` 会替换为最终端口。

控制器启动时会在后台预热默认镜像。镜像未就绪时，创建 sandbox 会立即返回
`503 image preparing`，不会占住调用直到工具超时；镜像拉取完成后重试即可。

默认 `ubuntu:22.04` 只提供基础 shell，不保证预装 Python、Node.js 或 pip。需要代码
运行环境时，可先通过终端安装，例如：

```bash
apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y python3 python3-pip
```

其他融合命令：

```bash
docker-control status
docker-control config
docker-control version
docker-control help
```

## 通用 Compose

```bash
export DOCKER_CONTROL_TOKEN="$(openssl rand -hex 32)"
export DOCKER_CONTROL_IMAGE=ubuntu:22.04
docker compose up -d --build
```

默认只将管理端口发布到 `127.0.0.1:16544`。其他容器化客户端可加入控制网络，或按其
部署架构调整端口和网络。

AstrBot 仅是示例适配器之一：

```text
examples/astrbot/compose.yml
```

## Linux systemd

不需要解压额外安装脚本，Linux 二进制可直接自安装：

```bash
chmod +x docker-control-v3.4.0-linux-amd64
sudo ./docker-control-v3.4.0-linux-amd64 install \
  --port 16544 \
  --image ubuntu:22.04
```

默认以执行脚本时的 `$PWD` 为部署根目录，所有项目文件均写入：

```text
$PWD/.docker-control/bin/docker-control
$PWD/.docker-control/docker-control.env
$PWD/.docker-control/docker-control.service
$PWD/.docker-control/runtime.json
```

需要指定其他项目目录时使用 `--home`：

```bash
sudo ./docker-control-v3.4.0-linux-amd64 install --home /srv/docker-control
```

脚本不会把项目文件复制到 `/usr/local`、`/etc/docker-control` 或 `/run`。为了实现系统
开机自启，`systemctl link` 会在 systemd 系统目录中创建一个指向
`$PWD/.docker-control/docker-control.service` 的链接；该链接不是项目文件副本。

```bash
systemctl status docker-control
journalctl -u docker-control -f
```

从旧 AstrBot 命名版本迁移：

```bash
sudo ./docker-control-v3.4.0-linux-amd64 install --migrate-legacy
```

详见 [MIGRATION.md](MIGRATION.md)。

卸载默认会停止并卸载 systemd、自 Docker Engine 删除所有新旧标签的受管 sandbox，
再验证安装标记并删除 `$PWD/.docker-control`：

```bash
sudo ./.docker-control/bin/docker-control uninstall
```

可选保留项：

```bash
sudo ./.docker-control/bin/docker-control uninstall --keep-containers
sudo ./.docker-control/bin/docker-control uninstall --keep-files
```

为兼容旧流程，[install-systemd.sh](install-systemd.sh) 仍保留，但只负责选择当前
CPU 架构的二进制并转发到 `docker-control install`。

## 主要配置

```text
DOCKER_CONTROL_TOKEN
DOCKER_CONTROL_LISTEN=0.0.0.0:16544
DOCKER_CONTROL_SOCKET=/var/run/docker.sock
DOCKER_CONTROL_IMAGE=ubuntu:22.04
DOCKER_CONTROL_PUBLIC_ENDPOINT=ws://host.docker.internal:{port}
DOCKER_CONTROL_RUNTIME_FILE=runtime.json
CONTAINER_NAME_PREFIX=sandbox
SANDBOX_NETWORK=bridge
TERMINAL_IDLE_SECONDS=300
MAX_TERMINAL_WINDOWS=40
MAX_CONTAINERS=20
```

v2 环境变量仍可读取，便于滚动迁移。

## 构建与验证

```powershell
.\build-release.ps1 -Version v3.4.0
```

```bash
./build-release.sh v3.4.0
go test ./...
go vet ./...
```

静态发布物位于 `dist/`。
