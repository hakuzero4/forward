# forward — HTTP 转发服务

> 让无法直连外网的设备（例如主路由）通过一个中转接口访问任意 http/https 地址，并支持 Telegram 通知、转发记录与可视化页面。

[![Build & Push Docker Image](https://github.com/hakuzero4/forward/actions/workflows/docker-image.yml/badge.svg)](https://github.com/hakuzero4/forward/actions/workflows/docker-image.yml)

## 特性

- **通用转发** `POST /api/forward`：转发到任意 http/https 地址，返回目标的状态码、响应头和响应体
- **Telegram 专用** `POST /api/send`：文本、图片 URL、话题线程、Markdown/HTML、静默发送
- **转发记录**：内存环形 + JSONL 文件持久化，重启不丢，Token 自动打码
- **内置页面**：日志展示（筛选 / 自动刷新 / 清空）+ 发送面板 + 使用说明
- **安全**：可选鉴权 `RELAY_AUTH_TOKEN`、域名白/黑名单、请求体大小限制
- **零依赖**：纯 Go 标准库，单一静态二进制；Docker 镜像支持 amd64 / arm64 / arm/v7

## 使用场景

主路由、内网设备无法直连 Telegram 或其他外网地址，但局域网内某台机器可以。把本服务跑在那台可联网的机器上，其他设备通过 HTTP 请求本服务即可代为访问任意地址：

```
主路由 / 内网设备 ──curl──▶ forward 中继 ──▶ 任意 http/https 地址（如 Telegram）
```

## 快速开始

### 本地运行

```bash
export TELEGRAM_BOT_TOKEN=123456:ABC...   # Bot Token（必填）
export LOG_FILE=/var/log/forward.jsonl    # 可选：记录持久化
./forward
```

Windows PowerShell：

```powershell
$env:TELEGRAM_BOT_TOKEN = "123456:ABC..."
.\forward.exe
```

### Docker 运行

镜像发布在 GHCR：`ghcr.io/hakuzero4/forward`（推送到 `main` 自动构建，打 `v*` 标签发布版本号镜像）。

```bash
docker run -d --name forward -p 8080:8080 \
  -e TELEGRAM_BOT_TOKEN=123456:ABC... \
  -e RELAY_AUTH_TOKEN=your-secret \
  -e LOG_FILE=/data/forward.jsonl \
  -v forward-logs:/data \
  ghcr.io/hakuzero4/forward:latest
```

## 配置

通过环境变量或命令行参数配置：

| 环境变量 | 参数 | 说明 | 默认值 |
| --- | --- | --- | --- |
| `TELEGRAM_BOT_TOKEN` | `-bot-token` | Telegram Bot Token（必填） | - |
| `RELAY_AUTH_TOKEN` | `-auth-token` | 调用方鉴权 Token（建议设置） | 空 |
| `LISTEN_ADDR` | `-listen` | 监听地址 | `:8080` |
| `TELEGRAM_API_BASE` | `-api-base` | Telegram API 地址 | `https://api.telegram.org` |
| - | `-timeout` | 出站请求超时 | `30s` |
| `FORWARD_MAX_BODY` | `-forward-max-body` | 转发请求/响应体上限（字节） | `10485760` (10 MiB) |
| `FORWARD_ALLOW_HOSTS` | `-forward-allow-hosts` | `/api/forward` 域名白名单（逗号分隔） | 空 |
| `FORWARD_DENY_HOSTS` | `-forward-deny-hosts` | `/api/forward` 域名黑名单（逗号分隔） | 空 |
| `LOG_FILE` | `-log-file` | 记录 JSONL 文件路径（空=仅内存） | 空 |
| `LOG_MAX` | `-log-max` | 内存保留记录数 | `1000` |

白/黑名单匹配规则：`example.com` 同时匹配 `example.com` 及其子域名（如 `api.example.com`）。

## 接口

### 发 Telegram 消息 — `POST /api/send`

```bash
curl -X POST http://127.0.0.1:8080/api/send \
  -H "Content-Type: application/json" \
  -d '{"chat_id":"-1001234567890","text":"Hello from router"}'
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `chat_id` | 是 | 群组/用户 ID，群组以 `-100` 开头 |
| `text` | 二选一 | 消息文本 |
| `photo_url` | 二选一 | 图片 URL，走 `sendPhoto` |
| `caption` | 否 | 图片说明，缺省用 `text` |
| `message_thread_id` | 否 | 话题线程 ID（forum topic） |
| `parse_mode` | 否 | `MarkdownV2` 或 `HTML` |
| `disable_web_page_preview` / `disable_notification` | 否 | 关闭链接预览 / 静默发送 |

### 转发任意地址 — `POST /api/forward`

```bash
curl -X POST http://127.0.0.1:8080/api/forward \
  -H "Content-Type: application/json" \
  -d '{"url":"https://example.com/status","method":"GET"}'
```

简单 GET 也可用：`curl "http://127.0.0.1:8080/api/forward?url=https://example.com/status"`

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `url` | 是 | 目标地址，仅支持 http/https |
| `method` | 否 | GET / POST / PUT / PATCH / DELETE，默认 GET |
| `headers` | 否 | 额外请求头（Host 也可指定） |
| `body` | 否 | 请求体字符串 |
| `timeout_ms` | 否 | 本次超时（毫秒，上限 120000） |

返回：`{"ok":true,"status":<上游状态码>,"headers":{...},"body":"..."}`

### 转发记录

- `GET /api/logs?limit=100&kind=forward&status=502`：查询记录，`limit` 默认 100、上限 5000，`kind` 为 `send` 或 `forward`
- `DELETE /api/logs`：清空记录（内存和日志文件）
- 打开 `http://127.0.0.1:8080/`（或 `/logs`）：HTML 页面，含日志展示、发送面板、使用说明

### 健康检查

`GET /healthz`

## 鉴权

设置 `RELAY_AUTH_TOKEN` 后，`/api/send` 和 `/api/forward` 必须带 `X-Relay-Token` 或 `Authorization: Bearer <token>`：

```bash
curl -X POST http://127.0.0.1:8080/api/forward \
  -H "Content-Type: application/json" \
  -H "X-Relay-Token: your-secret" \
  -d '{"url":"https://example.com/status"}'
```

页面发送面板也可以填写 Token，会自动保存在浏览器（localStorage）。

## 从源码构建

```bash
go build -o forward .
# 或
docker build -t forward .
```

## 开发

```bash
go vet ./...
go test ./...
```

## 注意事项

- `/api/forward` 相当于开放转发代理，务必设置 `RELAY_AUTH_TOKEN`；需要限制目标时用 `FORWARD_ALLOW_HOSTS`。
- 日志中的 Bot Token 和 `token` / `access_token` 参数会自动打码；日志页面与 `/api/logs` 默认不要求鉴权，建议只在内网监听或加反向代理。
- 中继机自身需要代理时设置 `HTTPS_PROXY`（Go 自动读取）。
- 响应体以字符串返回，适合 JSON/文本接口；二进制内容建议自行 base64。