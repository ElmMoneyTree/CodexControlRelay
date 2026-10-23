# Codex Control Relay for CentOS/RHEL x86_64

此包包含静态链接的 Linux `amd64` Relay，不需要在服务器安装 Go、Node.js 或 SQLite。默认只监听 `127.0.0.1:8787`，需要由同机 Nginx/Caddy 反向代理并提供公网 HTTPS。

## 安装

安装前准备一个已经解析到服务器的域名，例如 `relay.example.com`。解压后运行：

```bash
tar -xzf codex-relay-centos-linux-amd64-c46a45c.tar.gz
cd codex-relay-centos-linux-amd64-c46a45c
sudo ./install.sh https://relay.example.com
```

安装程序会：

- 创建受限的 `codex-relay` 系统用户；
- 安装二进制到 `/opt/codex-relay/codex-relay`；
- 安装配置到 `/etc/codex-relay/relay.yaml`；
- 首次安装时生成 64 位十六进制管理令牌；
- 将 SQLite 数据保存在 `/var/lib/codex-relay/relay.sqlite`；
- 注册并启动 `codex-relay.service`。

升级安装会保留已有的 Relay 配置、管理令牌和数据库。

## 配置公网 HTTPS

包内提供 `nginx.conf.example`。把其中的域名替换为你的域名，并配置实际证书路径。若 CentOS 开启 SELinux 且 Nginx 无法连接 Relay，可启用 HTTP 进程的网络连接权限：

```bash
sudo setsebool -P httpd_can_network_connect 1
```

防火墙只需要开放反向代理使用的 `443/tcp`，不要把 Relay 的 `8787` 端口直接暴露到公网。

## 验证

```bash
curl -fsS http://127.0.0.1:8787/health
curl -fsS http://127.0.0.1:8787/ready
sudo systemctl status codex-relay --no-pager
sudo journalctl -u codex-relay -n 100 --no-pager
```

读取手机端 Controller 需要的管理令牌：

```bash
sudo sed -n 's/^CODEX_RELAY_CONTROL_TOKEN=//p' /etc/codex-relay/relay.env
```

请像保护管理员密码一样保护此令牌。当前 Relay v1 的 `ownerId` 只是数据分组字段，不是独立的鉴权边界。

## 常用维护

修改配置后先校验，再重启：

```bash
sudo bash -c 'set -a; source /etc/codex-relay/relay.env; set +a; /opt/codex-relay/codex-relay --config /etc/codex-relay/relay.yaml --check-config'
sudo systemctl restart codex-relay
```

数据库位于 `/var/lib/codex-relay/relay.sqlite`。离线备份前先停止服务；若在线复制，必须使用 SQLite 备份机制，不能只复制主文件而忽略 WAL。
