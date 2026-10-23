#!/usr/bin/env bash
# 云浏览器（Neko v3 + Chromium）一键部署
# 用法一（推荐）：curl -fsSL https://raw.githubusercontent.com/cesar699/browser-box/main/deploy.sh | sudo bash
# 用法二：把整个仓库目录放到 VPS 上，进目录执行  bash deploy.sh
# 重复执行 = 更新（拉新镜像并重启），.env 与 profile 数据保留。
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null || true

# 经 curl 管道执行时本机还没有仓库文件：先克隆/更新到 /opt/browser-box 再正式执行
if [ ! -f docker-compose.yml ]; then
  command -v git >/dev/null 2>&1 || { apt-get update -qq && apt-get install -y -qq git; }
  if [ -d /opt/browser-box/.git ]; then
    git -C /opt/browser-box pull --ff-only
  else
    git clone https://github.com/cesar699/browser-box.git /opt/browser-box
  fi
  cd /opt/browser-box
  exec bash deploy.sh "$@"
fi

say() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m错误：\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || SUDO="sudo" ; SUDO="${SUDO:-}"

# ---------- 1. Docker ----------
if ! command -v docker >/dev/null 2>&1; then
  say "未检测到 Docker，正在安装…"
  curl -fsSL https://get.docker.com | $SUDO sh
  $SUDO systemctl enable --now docker
fi
docker compose version >/dev/null 2>&1 || die "docker compose 插件不可用，请更新 Docker 后重试"

# ---------- 2. .env ----------
gen_pw() { openssl rand -base64 24 | tr -dc 'A-Za-z0-9' | cut -c1-20; }
detect_ip() {
  for u in https://api.ipify.org https://ifconfig.me/ip https://icanhazip.com; do
    ip="$(curl -fsS -m 8 "$u" 2>/dev/null | tr -d ' \n' || true)"
    case "$ip" in *.*.*.*) echo "$ip"; return 0;; esac
  done
  return 1
}

if [ ! -f .env ]; then
  say "生成 .env（随机密码 + 自动探测公网 IP）…"
  PUBLIC_IP="$(detect_ip)" || die "公网 IP 自动探测失败，请手动复制 .env.example 为 .env 并填写 PUBLIC_IP"
  cat > .env <<EOF
WEB_BIND=0.0.0.0
WEB_PORT=8080
PUBLIC_IP=${PUBLIC_IP}
NEKO_USER_PASSWORD=$(gen_pw)
NEKO_ADMIN_PASSWORD=$(gen_pw)
NEKO_SCREEN=1280x720@30
EOF
  chmod 600 .env
else
  say ".env 已存在，沿用现有配置（更新模式）"
fi
# shellcheck disable=SC1091
set -a; . ./.env; set +a
[ -n "${PUBLIC_IP:-}" ] || die ".env 里 PUBLIC_IP 为空，WebRTC 需要它，请填入 VPS 公网 IP 后重跑"
[ "${NEKO_ADMIN_PASSWORD}" != "change-me-admin" ] || die ".env 里还是示例密码，请改掉后重跑"

# ---------- 3. 防火墙（本机 ufw；云安全组需另行放行，见结尾提醒） ----------
if command -v ufw >/dev/null 2>&1 && $SUDO ufw status 2>/dev/null | grep -qi "Status: active"; then
  say "ufw 已启用，放行 ${WEB_PORT}/tcp 与 52000-52100/udp…"
  $SUDO ufw allow "${WEB_PORT}/tcp" >/dev/null || true
  $SUDO ufw allow 52000:52100/udp >/dev/null || true
fi

# ---------- 4. 启动 ----------
mkdir -p profile
# 容器里的 neko 用户是 uid 1000；profile 若归 root 所有，Chromium 写不了资料目录会崩溃循环
# （本地实测：root 所有时 crashpad 反复 SIGTRAP，改归 1000 后正常）
$SUDO chown -R 1000:1000 profile
say "拉取镜像并启动…"
docker compose pull
docker compose up -d

# ---------- 5. 健康检查 ----------
say "等待服务响应（最长 90 秒）…"
ok=0
for _ in $(seq 1 45); do
  if curl -fsS -m 3 "http://127.0.0.1:${WEB_PORT}/" >/dev/null 2>&1; then ok=1; break; fi
  sleep 2
done
[ "$ok" -eq 1 ] || { docker compose logs --tail 50 neko || true; die "服务未起来，上面是最近日志"; }

cat <<EOF

============================================================
 云浏览器已启动 ✔

 访问地址（先用 HTTP 测试）：http://${PUBLIC_IP}:${WEB_PORT}
 管理员密码：${NEKO_ADMIN_PASSWORD}
 访客密码：  ${NEKO_USER_PASSWORD}
 （密码也在 .env 里，登录后进入的是跑在 VPS 上的 Chromium）

 ★ 最容易翻车的一步：去云厂商控制台的安全组里放行 UDP 52000-52100。
   没放行时表现为：能打开登录页、能登录，但画面黑屏/一直转圈。
 ★ 长期使用请配 HTTPS：目录里给了 nginx-site.conf 和 Caddyfile.snippet，
   把子域名解析到本机后套上，再把 .env 的 WEB_BIND 改为 127.0.0.1 重跑本脚本。
============================================================
EOF
