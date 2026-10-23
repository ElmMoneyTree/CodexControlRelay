#!/usr/bin/env python3
"""Create a Relay account from its server terminal without printing credentials."""

import argparse
import getpass
import json
import os
import shlex
import sys
import urllib.error
import urllib.request
from pathlib import Path


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def read_admin_token(env_file):
    # Read the installer's KEY=value file without executing shell commands.
    values = {}
    for line in Path(env_file).read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("export "):
            line = line[7:].lstrip()
        key, separator, raw = line.partition("=")
        if separator and key.strip() in ("CODEX_RELAY_CONTROL_TOKEN", "CONTROL_TOKEN"):
            parts = shlex.split(raw, comments=True)
            if len(parts) != 1:
                raise ValueError("管理令牌配置格式无效。")
            values[key.strip()] = parts[0]
    token = values.get("CODEX_RELAY_CONTROL_TOKEN") or values.get("CONTROL_TOKEN")
    if not token or len(token) < 32:
        raise ValueError("配置文件中没有有效的管理令牌。")
    return token


def create_account(email, display_name, password, token, port):
    payload = json.dumps({
        "email": email,
        "displayName": display_name,
        "password": password,
        "personalWorkspaceName": "我的空间",
    }).encode("utf-8")
    request = urllib.request.Request(
        "http://127.0.0.1:{}/control/accounts".format(port),
        data=payload,
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"},
        method="POST",
    )
    # Keep administrator credentials on this server, including when proxy
    # environment variables or an unexpected redirect are present.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(request, timeout=20) as response:
        if response.status != 201:
            raise ValueError("服务未确认账号创建成功（HTTP {}）。".format(response.status))
        # The response contains a session token; deliberately never display it.
        response.read()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--email", required=True)
    parser.add_argument("--display-name", default="ilang")
    parser.add_argument("--env-file", default="/etc/codex-relay/relay.env")
    parser.add_argument("--port", type=int, default=8787)
    args = parser.parse_args()
    email = args.email.strip().lower()
    if "@" not in email or not 1 <= args.port <= 65535:
        parser.error("邮箱或端口无效。")
    try:
        token = read_admin_token(args.env_file)
        # Never fall back to echoed input when no interactive terminal exists.
        with open("/dev/tty", "w") as terminal:
            if not os.isatty(terminal.fileno()):
                raise ValueError("请在服务器的交互式终端运行此命令。")
            print("创建 Codex Control 账号：{}".format(email))
            password = getpass.getpass("设置密码（至少 12 个字符，输入不显示）：", stream=terminal)
            if len(password) < 12 or len(password.encode("utf-8")) > 1024:
                raise ValueError("密码至少需要 12 个字符，且不能超过 1024 字节。")
            confirmation = getpass.getpass("再次输入密码：", stream=terminal)
            if password != confirmation:
                raise ValueError("两次密码不一致，未创建账号。")
        create_account(email, args.display_name, password, token, args.port)
        print("账号创建成功：{}。现在可以在手机使用这个邮箱和刚设置的密码登录。".format(email))
        return 0
    except urllib.error.HTTPError as error:
        messages = {
            400: "账号信息不符合服务器要求。",
            401: "服务器管理令牌无效，请核对配置。",
            403: "服务器拒绝创建账号。",
            404: "服务器没有账号管理接口，请核对运行中的 Relay 版本及端口。",
            409: "这个邮箱已有账号，未修改其密码。",
            429: "请求过于频繁，请稍后重试。",
        }
        print(messages.get(error.code, "服务返回 HTTP {}，未能确认创建成功。".format(error.code)), file=sys.stderr)
    except (urllib.error.URLError, TimeoutError):
        print("连接未完成；请检查本机 Relay。请求可能已经到达服务器，可先尝试登录，重试不会覆盖已有账号。", file=sys.stderr)
    except (OSError, ValueError) as error:
        if isinstance(error, ValueError):
            print(str(error), file=sys.stderr)
        else:
            print("无法读取管理配置或终端，请在 Relay 服务器通过 sudo 运行。", file=sys.stderr)
    except (EOFError, KeyboardInterrupt):
        print("已取消。", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
