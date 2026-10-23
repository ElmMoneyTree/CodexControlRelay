# CodexControlRelay

图片使用 `/v1/attachments`（旧版 `/control/attachments`）按需转发，参数为空间或 owner、设备、任务、消息、图片 ID 及 offset/length。每块最多 192 KiB、单图最多 25 MiB，经同一 Agent 请求通道返回经过校验的 base64 数据、MIME、总长度与 SHA-256。图片及图片索引不写入 Relay 文件、数据库或命令队列；手机保存文件和 SQLite 关联信息，并负责离线查看及随对话删除。设备、任务、空间授权会在请求前与返回前分别检查。

Codex Control 的独立 Go 连接服务，负责账号、个人/团队空间、设备认领、状态同步和命令转发。使用 SQLite，支持 HTTPS、反向代理、Docker 和 systemd，并保留原 Node Relay API 与数据库的迁移兼容能力。

本服务不包含控制界面，不在服务器上执行 Codex 任务。Controller 和设备 Agent 需要单独安装；Relay 无需 Node.js、pnpm 或 Electron。

聊天历史由手机本地 SQLite 保存。Relay 的 `/v1/messages`（旧版 `/control/messages`）只转发当次手机读取请求：Agent 从 `/agent/conversation-requests` 领取请求，读取电脑日志后向 `/agent/conversation-responses/{id}` 回复。请求和响应只短暂保留在内存中，交付、断开或 12 秒超时后释放；不进入命令数据库或日志。手机自行保存同步游标、分页与删除标记，旧批量上传接口 `/agent/messages` 已移除。

升级会删除实验版 `conversation_messages` 表及已存的回复摘要、最新回复正文；任务状态只持久保存用于列表和授权的元数据。发往电脑的消息仍通过原有可靠命令队列临时保存，交付完成或失败后清除正文，成功回执只保留投递状态。这些运行数据不作为聊天历史。新消息的首次读取需要电脑在线，离线查看、删除整段对话及清空全部对话均在手机完成。

手机可通过 `/v1/projects?workspaceId=...&deviceId=...` 读取所选电脑当前可用的聊天类型和项目：响应为 `{backends:["codex","chatgpt"],projects:[{id,name,kind}]}`，`kind` 对应 `codex` 或 `chatgpt`。项目目录通过 `/agent/project-requests` 和 `/agent/project-responses/{id}` 按需转发，仅在内存等待至多 12 秒，不写入 Relay 数据库。旧版入口为 `/control/projects?ownerId=...&deviceId=...`。

创建聊天使用原有可靠命令队列：向 `/v1/commands?workspaceId=...` 提交 `{deviceId,kind:"create_thread",backend:"codex"或"chatgpt",projectId?:"所选项目ID",text:"首条消息",dedupeKey:"本次创建唯一且重试不变的标识"}`。省略 `projectId` 创建项目外聊天，创建时不传 `threadId`。电脑会重新校验项目归属并执行创建，完成回执仅保留 `{submitted:true,threadId,backend,projectId?}`；如果桌面仍在异步创建，回执改为 `{submitted:true,clientThreadId,status:"starting",backend,projectId?}`，等待任务列表出现正式聊天，不重新提交创建命令。失败或完成后清除首条消息正文。账号空间成员权限、设备归属及重复提交保护与其他命令一致。任务列表可带 `backend`，回复命令 `send_message` 也支持 `backend`，旧客户端省略时继续按 Codex 处理。

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

## 首次创建账号

`CODEX_RELAY_CONTROL_TOKEN` 现在只用于服务器管理和旧版兼容，不能分发给手机用户。部署后先在服务器本机创建首个账号：

```bash
curl --fail-with-body http://127.0.0.1:8787/control/accounts \
  -H "Authorization: Bearer $CODEX_RELAY_CONTROL_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"email":"owner@example.com","displayName":"Owner","password":"replace-with-at-least-12-characters","personalWorkspaceName":"我的空间"}'
```

如需允许用户自行注册，将 `security.allow_registration` 设为 `true`；正式公开服务建议在接入邮箱验证或邀请机制前保持关闭。

## 连接客户端

正常连接不再填写 Owner ID、管理令牌或配对码：

1. 手机 App 使用账号登录统一连接服务。
2. Windows Codex 插件首次运行时生成 Ed25519 设备身份，并打开五分钟有效的认领页面。
3. 电脑认领页仅显示二维码；用已登录的 App 扫码，在手机上核对设备指纹并选择个人/团队空间。
4. Relay 原子绑定租户，向插件下发仅能操作本设备的令牌和目标 Relay。
5. 插件自动开始同步，App 可继续查看任务、发送消息并处理审批。

详细协议、安全边界、角色权限与旧设备原位迁移见 [设备认领设计](./DEVICE-CLAIM.md)。

旧版 Controller 与数字配对仍可在迁移期使用。迁移完成后将 `security.legacy_api_enabled` 设为 `false`，旧 Agent 仍可同步并可通过设备认领原位升级，但不能再新建旧版配对或使用全局控制接口。

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
| App 登录返回 401 | 核对账号密码；服务器会话被撤销或过期后需重新登录 |
| 认领链接失效 | 在插件中重新发起；认领会话只有五分钟且只能批准一次 |
| 扫码后设备未上线 | 在插件中查看状态；确认目标 Relay 已由 Nginx 完整代理 `/v1/` 与 `/agent/` |
| 旧版配对返回 404 | `legacy_api_enabled` 已关闭；改用设备认领或在迁移期临时开启 |
| 数据库无法打开 | 检查数据目录写权限；systemd 使用 /var/lib/codex-relay |
| 请求返回 429 | 等待限流窗口恢复，检查可信代理与转发头 |
| systemd 启动失败 | 用 sudo journalctl -u codex-relay -n 100 --no-pager 查看日志 |

## License

采用 [MIT License](./LICENSE)，保留原项目版权声明。第三方依赖使用各自许可证。
