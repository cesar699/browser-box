#!/usr/bin/env bash
# browser-box v2 一键部署（自研版：单个 Go 程序 + Chrome for Testing，无 Docker）
# 用法：curl -fsSL https://raw.githubusercontent.com/cesar699/browser-box/main/deploy.sh | sudo bash
# 重复执行 = 更新（拉最新源码重新编译并重启），密码与浏览器资料保留。
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null || true

REPO="https://github.com/cesar699/browser-box.git"
INSTALL_DIR="/opt/browser-box"
GO_VERSION="1.25.5"
CHROME_VERSION="156.0.8078.47"
PORT="${PORT:-8080}"

say() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m错误：\033[0m %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || SUDO="sudo"; SUDO="${SUDO:-}"

# 经 curl 管道执行时本机还没有仓库：先克隆/更新到 /opt/browser-box 再正式执行
if [ ! -f go.mod ]; then
  command -v git >/dev/null 2>&1 || { apt-get update -qq && apt-get install -y -qq git; }
  if [ -d "$INSTALL_DIR/.git" ]; then
    git -C "$INSTALL_DIR" pull --ff-only
  else
    $SUDO git clone "$REPO" "$INSTALL_DIR"
  fi
  cd "$INSTALL_DIR"
  exec $SUDO bash deploy.sh "$@"
fi

# ---------- 0. 停掉 v1 的 Neko 容器（占着同一个 8080 端口） ----------
if command -v docker >/dev/null 2>&1 && docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx cloud-browser; then
  say "检测到 v1（Neko）容器 cloud-browser，停用并移除（v2 不再需要它）…"
  docker rm -f cloud-browser >/dev/null 2>&1 || true
fi

# ---------- 1. Go 工具链 ----------
if ! command -v go >/dev/null 2>&1 && [ ! -x /usr/local/go/bin/go ]; then
  say "安装 Go ${GO_VERSION}…"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz
  $SUDO tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
fi
export PATH="/usr/local/go/bin:$PATH"
command -v go >/dev/null 2>&1 || die "Go 安装失败"
say "Go: $(go version)"

# ---------- 2. Chrome for Testing（独立版，无 snap 无 Docker） ----------
if [ ! -x chrome-linux64/chrome ]; then
  say "下载 Chrome for Testing ${CHROME_VERSION}…"
  curl -fsSL "https://storage.googleapis.com/chrome-for-testing-public/${CHROME_VERSION}/linux64/chrome-linux64.zip" -o /tmp/chrome.zip
  command -v unzip >/dev/null 2>&1 || { apt-get update -qq && apt-get install -y -qq unzip; }
  unzip -q -o /tmp/chrome.zip && rm -f /tmp/chrome.zip
  chmod +x chrome-linux64/chrome chrome-linux64/chrome_crashpad_handler chrome-linux64/chrome_sandbox 2>/dev/null || true
fi
./chrome-linux64/chrome --version >/dev/null 2>&1 || die "Chrome 无法运行（缺系统库时执行：apt-get install -y libnss3 libatk1.0-0 libatk-bridge2.0-0 libcups2 libdrm2 libxkbcommon0 libxcomposite1 libxdamage1 libxfixes3 libxrandr2 libgbm1 libasound2t64 libpango-1.0-0 libcairo2）"
say "Chrome: $(./chrome-linux64/chrome --version)"

# ---------- 2.5 音频组件（声音功能用：虚拟声卡 + 编码器） ----------
if ! command -v ffmpeg >/dev/null 2>&1 || ! command -v pulseaudio >/dev/null 2>&1; then
  say "安装音频组件 ffmpeg + pulseaudio…"
  apt-get update -qq && apt-get install -y -qq --no-install-recommends ffmpeg pulseaudio || say "音频组件安装失败：浏览器照常可用，只是没有声音"
fi

# ---------- 3. 编译 ----------
say "编译 browser-boxd…"
go build -trimpath -o browser-boxd .

# ---------- 4. 配置（密码只生成一次，存 browser-box.env） ----------
if [ ! -f browser-box.env ]; then
  PW="$(openssl rand -base64 24 | tr -dc 'A-Za-z0-9' | cut -c1-20)"
  cat > browser-box.env <<EOF
BROWSERBOX_PASSWORD=${PW}
EOF
  chmod 600 browser-box.env
  say "已生成登录密码（存于 browser-box.env）"
else
  say "沿用已有 browser-box.env（更新模式，密码不变）"
fi
set -a; . ./browser-box.env; set +a

# ---------- 5. systemd 服务 ----------
say "写入 systemd 服务…"
$SUDO tee /etc/systemd/system/browser-box.service >/dev/null <<EOF
[Unit]
Description=browser-box remote browser
After=network.target

[Service]
Type=simple
WorkingDirectory=${INSTALL_DIR}
EnvironmentFile=${INSTALL_DIR}/browser-box.env
ExecStart=${INSTALL_DIR}/browser-boxd -listen :${PORT}
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
$SUDO systemctl daemon-reload
$SUDO systemctl enable browser-box >/dev/null 2>&1 || true
$SUDO systemctl restart browser-box

# ---------- 6. 健康检查 ----------
say "等待服务响应（最长 60 秒）…"
ok=0
for _ in $(seq 1 30); do
  if curl -fsS -m 3 "http://127.0.0.1:${PORT}/api/me" >/dev/null 2>&1; then ok=1; break; fi
  sleep 2
done
if [ "$ok" -ne 1 ]; then
  $SUDO journalctl -u browser-box -n 40 --no-pager || true
  die "服务未起来，上面是最近日志"
fi

PUBLIC_IP="$(curl -fsS -m 8 https://api.ipify.org 2>/dev/null || curl -fsS -m 8 https://ifconfig.me/ip 2>/dev/null || echo '<服务器IP>')"
cat <<EOF

============================================================
 browser-box 已启动 ✔（自研版，无 Docker）

 访问地址：http://${PUBLIC_IP}:${PORT}
 登录密码：${BROWSERBOX_PASSWORD}
 （只有密码、没有用户名；密码也在 ${INSTALL_DIR}/browser-box.env）

 只需放行一个 TCP 端口 ${PORT}，没有 UDP、没有 NAT 配置。
 换密码：改 browser-box.env 后执行 systemctl restart browser-box
 看日志：journalctl -u browser-box -f
============================================================
EOF
