# CodexControlRelay

Codex Control 的独立 Go Relay，负责 Controller 与 Agent 之间的设备配对、状态同步和命令转发。使用 SQLite，支持 HTTPS、反向代理、Docker 和 systemd，并兼容原 Node Relay 的 API 与数据库。

本服务不包含控制界面，不在服务器上执行 Codex 任务。Controller 和设备 Agent 需要单独安装；Relay 无需 Node.js、pnpm 或 Electron。

## 构建

需要 Go 1.26.0 或更高版本。以下命令在独立仓库根目录执行：

```bash
git clone https://github.com/ElmMoneyTree/CodexControlRelay.git
cd CodexControlRelay
go mod download
go test ./...
go build -trimpath -o codex-relay ./cmd/codex-relay
```

Windows 构建使用 `go build -trimpath -o codex-relay.exe ./cmd/codex-relay`。

## 本机启动

Linux / macOS：

```bash
cp relay.example.yaml relay.yaml
export CODEX_RELAY_CONTROL_TOKEN="$(openssl rand -hex 32)"
export CODEX_RELAY_PUBLIC_URL="http://127.0.0.1:8787"
./codex-relay --check-config
./codex-relay
```

Windows PowerShell（兼容 5.1）：

```powershell
Copy-Item relay.example.yaml relay.yaml
$tokenBytes = New-Object byte[] 32
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
$rng.GetBytes($tokenBytes)
$rng.Dispose()
$env:CODEX_RELAY_CONTROL_TOKEN = -join ($tokenBytes | ForEach-Object { $_.ToString('x2') })
$env:CODEX_RELAY_PUBLIC_URL = 'http://127.0.0.1:8787'
.\codex-relay.exe --check-config
.\codex-relay.exe
```

另开终端访问 `http://127.0.0.1:8787/health` 和 `http://127.0.0.1:8787/ready`，确认服务与数据库就绪。默认读取当前目录的 `relay.yaml`，也可用 `--config /path/to/relay.yaml` 指定。数据库及证书的相对路径以配置文件目录为基准。

环境变量只保存在当前终端。正式部署请持久保存管理令牌，将公开地址改为实际 HTTPS 地址；不要每次重启都重新生成令牌。

## 连接客户端

1. Controller 配置 Relay 地址和与服务器一致的管理令牌 `CODEX_RELAY_CONTROL_TOKEN`。
2. Controller 生成一次性配对码，默认有效期 10 分钟。
3. 运行 Codex 的设备上配置 Agent，填入相同 Relay 地址和配对码。
4. Agent 注册后保存自己的设备令牌，返回 Controller 查看设备在线状态并操作任务。

公网客户端使用实际 HTTPS 地址，不能使用服务器的 `127.0.0.1`。管理令牌用于 Controller；Agent 使用配对后获得的设备令牌。`ownerId` 是状态分组字段，不是独立租户的鉴权边界。

API 连接检查（Bash，在已设置令牌的终端中执行）：

```bash
curl --fail-with-body http://127.0.0.1:8787/control/state \
  -H "Authorization: Bearer $CODEX_RELAY_CONTROL_TOKEN"
curl --fail-with-body http://127.0.0.1:8787/control/pairing-codes \
  -H "Authorization: Bearer $CODEX_RELAY_CONTROL_TOKEN" \
  -H 'Content-Type: application/json' -d '{"ownerId":"owner"}'
```

## 公网部署

完整参数、Linux systemd、Nginx、直接 TLS、数据迁移与备份见 [部署指南](./DEPLOYMENT.md)。完整字段见 [配置模板](./relay.example.yaml)。

### Docker Compose

宿主机需要 Docker 和 Compose。先将 `deploy/relay.docker.yaml` 中的 `public_url` 改成真实 HTTPS 地址，然后执行：

```bash
cd deploy
export CODEX_RELAY_CONTROL_TOKEN="$(openssl rand -hex 32)"
docker compose -f docker-compose.example.yaml up -d --build
docker compose -f docker-compose.example.yaml logs -f relay
```

默认只绑定宿主机 `127.0.0.1:8787`，由宿主机 Nginx/Caddy 终止 HTTPS。数据库保存在 `relay-data` 命名卷中。

可将 `CODEX_RELAY_CONTROL_TOKEN=你的随机令牌` 写入 `deploy/.env` 并限制文件权限，Compose 会自动读取；该文件已被 Git 忽略。终端环境变量优先于 `.env`。

更新时在 `deploy` 执行 `docker compose -f docker-compose.example.yaml up -d --build`，停止时使用 `docker compose -f docker-compose.example.yaml down`。`down` 保留数据卷，`down -v` 会删除数据库卷。

## 开发与验证

```bash
go test ./...
go vet ./...
go build -trimpath -o codex-relay ./cmd/codex-relay
./codex-relay --version
```

Windows 输出文件名改为 `codex-relay.exe`。测试覆盖配置、协议、HTTP 服务、SQLite 存储和命令行。Node/Go 跨实现 E2E 位于原始 Codex Control 工作区，不包含在本独立仓库中。

交叉编译（Bash）：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/linux-amd64/codex-relay ./cmd/codex-relay
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o dist/linux-arm64/codex-relay ./cmd/codex-relay
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o dist/windows-amd64/codex-relay.exe ./cmd/codex-relay
```

默认版本显示 `dev`，可通过 Go `-ldflags` 设置 `main.version`、`main.commit` 和 `main.date`。

## 常见问题

| 现象 | 处理 |
| --- | --- |
| 找不到配置 | 复制示例为 relay.yaml，或用 --config 指定路径 |
| 配置校验失败 | 检查 YAML 字段、至少 32 字符的令牌、监听地址和 TLS 文件 |
| 非回环 HTTP 被拒绝 | 公网使用 HTTPS，同机代理使用回环监听；容器使用 Docker 示例配置 |
| Controller 返回 401 | 核对管理令牌和 Authorization: Bearer 请求头 |
| Agent 配对失败 | 核对地址，重新生成未使用且未过期的配对码 |
| 数据库无法打开 | 检查数据目录写权限；systemd 使用 /var/lib/codex-relay |
| 请求返回 429 | 等待限流窗口恢复，检查可信代理与转发头 |
| systemd 启动失败 | 用 sudo journalctl -u codex-relay -n 100 --no-pager 查看日志 |

## License

采用 [MIT License](./LICENSE)，保留原项目版权声明。第三方依赖使用各自许可证。
