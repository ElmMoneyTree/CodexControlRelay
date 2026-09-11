# CodexControlRelay 部署指南

这是面向公网部署的 Relay 实现。它与现有 Node Relay 使用相同的 HTTP API 和 SQLite 表结构，因此现有 Controller、独立 Agent、Codex 插件 Agent 以及已有数据库都可以继续使用。

源码构建、本机启动和客户端连接请先阅读 [README](./README.md)。以下命令在独立仓库根目录执行。

## 配置

完整模板见 [relay.example.yaml](./relay.example.yaml)。YAML 使用严格字段检查，字段拼错会导致启动失败。

| 配置项 | 默认值 | 用途 |
| --- | --- | --- |
| `server.listen` | `127.0.0.1:8787` | HTTP/TLS 监听地址 |
| `server.public_url` | 空 | Controller 和 Agent 使用的外部 HTTPS origin |
| `server.tls.cert_file` / `key_file` | 空 | 由 Relay 直接终止 TLS 时使用 |
| `server.trusted_proxies` | 空 | 可以提供 `X-Forwarded-For` 的代理 IP/CIDR |
| `server.max_body_bytes` | `1000000` | 请求体上限 |
| `database.path` | `data/relay.sqlite` | SQLite 文件；相对配置文件解析 |
| `database.busy_timeout` | `5s` | SQLite 锁等待时间 |
| `security.control_token` | 空 | 控制端管理令牌，至少 32 个字符 |
| `security.control_token_env` | `CODEX_RELAY_CONTROL_TOKEN` | 读取令牌的环境变量名 |
| `security.allow_insecure_http` | `false` | 明确允许非回环地址上的明文 HTTP |
| `security.pairing_code_ttl` | `10m` | 一次性配对码有效期 |
| `security.offline_after` | `15s` | 设备判定离线的时间 |
| `security.allowed_origins` | 空 | 浏览器前端允许的精确 origin 列表 |
| `security.rate_limits.*` | 见模板 | 配对、控制端和 Agent 的按 IP 限流 |
| `logging.level` | `info` | `debug`、`info`、`warn` 或 `error` |
| `logging.format` | `json` | `json` 或 `text` |

支持下列环境变量：

- `CODEX_RELAY_CONFIG`
- `CODEX_RELAY_LISTEN`，或 `CODEX_RELAY_HOST` + `CODEX_RELAY_PORT`
- `CODEX_RELAY_PUBLIC_URL`
- `CODEX_RELAY_DB_PATH`
- `CODEX_RELAY_TLS_CERT_FILE` / `CODEX_RELAY_TLS_KEY_FILE`
- `CODEX_RELAY_CONTROL_TOKEN`，或者 `security.control_token_env` 指定的变量

为兼容旧版启动方式，也支持 `HOST`、`PORT`、`RELAY_DB_PATH` 和 `CONTROL_TOKEN`。部署脚本应优先使用 `CODEX_RELAY_*`。

## 公网部署

### 方式一：Nginx/Caddy 终止 HTTPS

推荐让 Relay 只监听 `127.0.0.1:8787`，由同机反向代理监听公网 443。使用 [relay.reverse-proxy.example.yaml](./deploy/relay.reverse-proxy.example.yaml) 与 [nginx.conf.example](./deploy/nginx.conf.example)：

```bash
sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin codex-relay
sudo install -d -m 0750 -o codex-relay -g codex-relay /etc/codex-relay /var/lib/codex-relay /opt/codex-relay
sudo install -m 0755 codex-relay /opt/codex-relay/codex-relay
sudo install -m 0640 -o root -g codex-relay deploy/relay.reverse-proxy.example.yaml /etc/codex-relay/relay.yaml
sudo nano /etc/codex-relay/relay.yaml
relay_token="$(openssl rand -hex 32)"
sudo install -m 0600 /dev/null /etc/codex-relay/relay.env
printf 'CODEX_RELAY_CONTROL_TOKEN=%s\n' "$relay_token" | sudo tee /etc/codex-relay/relay.env >/dev/null
unset relay_token
sudo install -m 0644 deploy/codex-relay.service /etc/systemd/system/codex-relay.service
sudo systemctl daemon-reload
sudo systemctl enable --now codex-relay
```

服务用户已存在时跳过 useradd；升级时保留原 relay.env，避免改变管理令牌。先按 README 构建 Linux 二进制，再执行安装。把示例域名替换为实际域名并为 Nginx 配置证书。反向代理必须覆盖客户端传入的转发头，Relay 的 `trusted_proxies` 只列出真实代理地址。示例已限制请求体并设置 HSTS。

### 方式二：Relay 直接终止 TLS

使用 [relay.direct-tls.example.yaml](./deploy/relay.direct-tls.example.yaml)，设置证书和私钥路径，然后运行：

```bash
CODEX_RELAY_CONTROL_TOKEN="$(openssl rand -hex 32)" \
  ./codex-relay --config /etc/codex-relay/relay.yaml
```

直接 TLS 模式可以监听 `0.0.0.0:8787`。防火墙只开放实际使用的 TLS 端口。systemd 服务使用 `codex-relay` 用户时，证书需可读，私钥建议设为 `root:codex-relay` 和 `0640`。不要将未加密的 Relay 端口暴露到公网。

### Docker Compose

[docker-compose.example.yaml](./deploy/docker-compose.example.yaml) 默认仅映射宿主机回环地址，供宿主机 Nginx/Caddy 代理：

```bash
cd deploy
export CODEX_RELAY_CONTROL_TOKEN="$(openssl rand -hex 32)"
docker compose -f docker-compose.example.yaml up -d --build
```

本仓库包含 Docker 构建所需文件。镜像按 Docker 目标架构构建，使用非 root 用户和纯 Go 静态二进制；SQLite 数据目录已预置为该用户所有，数据保存在命名卷 `relay-data`。

## 从 Node Relay 迁移

Go Relay 可以直接打开旧 Relay 的 SQLite 文件。迁移时避免两个进程同时写同一数据库：

1. 停止旧 Node/Electron Relay，让 SQLite WAL 正常关闭。
2. 备份 `relay.sqlite`；如果停机前仍存在 `-wal` 和 `-shm` 文件，连同主文件一起备份。
3. 在 Go 配置中把 `database.path` 指向该数据库，并使用原来的控制令牌。
4. 先运行 `codex-relay --config ... --check-config`，再启动服务。
5. 检查 `/ready`、Controller 设备列表和至少一个 Agent 的同步。

Go Relay 会自动把旧版 `pending_requests.turn_id NOT NULL` 表升级为可空版本，以支持 MCP 请求。回滚时先停止 Go Relay，再用同一个数据库启动 Node Relay 即可。跨实现兼容 E2E 位于原始 Codex Control 工作区，本独立仓库不包含该脚本。

## 健康检查与运维

```bash
curl -fsS https://relay.example.com/health
curl -fsS https://relay.example.com/ready
```

`/health` 证明 HTTP 进程可响应，`/ready` 还会检查 SQLite。两者不返回任务或凭据。

定期备份 SQLite。最稳妥的方法是先停止服务再复制主文件；在线备份需要使用 SQLite 的备份机制，不能只复制主文件而遗漏 WAL。数据库目录权限建议为 `0750`，数据库文件为 `0600`。控制令牌等同于 Controller 管理密码，应放在服务环境文件或密钥管理系统中，避免提交到仓库。

服务使用一次性配对码、仅保存设备令牌的 SHA-256、固定请求体上限、控制/Agent 鉴权、CORS 精确白名单、可信代理链解析、限流、TLS 1.2 下限和优雅停机。Relay 不保存任何 Codex 账号密码或会话 Cookie。

## 构建和验证

请参阅 [README 的开发与验证章节](./README.md#开发与验证)。本独立仓库直接使用 Go 命令，无需 pnpm。
