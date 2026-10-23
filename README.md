# 云浏览器（Neko + Chromium）

把一个真正的 Chromium 浏览器跑在你的 VPS 上：你在手机/电脑浏览器里打开一个页面，
看到的就是 VPS 上那个浏览器的实时画面（WebRTC 推流），点击、打字、滚动都传过去执行。
**所有网页请求都从 VPS 出口发出**，本地只收画面——适合"目标网站本地连不上、但服务器能连"的场景。
和反向代理的区别：JS、Cookie、CDN、登录都在服务器端真浏览器里原生跑，不存在改写不全的问题。

## 快速开始

前置：VPS 装有或能装 Docker（脚本会自动装）、内存 ≥2G、能放行端口。

一行部署（脚本会自动把仓库克隆到 `/opt/browser-box` 再执行）：

```bash
curl -fsSL https://raw.githubusercontent.com/cesar699/browser-box/main/deploy.sh | sudo bash
```

或手动：把本仓库克隆到 VPS，进目录执行 `bash deploy.sh`。

脚本会：装 Docker（如缺）→ 生成 `.env`（随机强密码 + 自动探测公网 IP）→ 尝试放行本机 ufw → 拉镜像启动 → 健康检查 → 打印访问地址和密码。

打开 `http://服务器IP:8080`，填任意显示名 + 密码（管理员密码能拿控制权），就进浏览器了。

## 必须手动做的一件事：云安全组放行 UDP

在云厂商控制台的安全组/防火墙里放行：

| 端口 | 用途 |
|---|---|
| TCP 8080（或你改的 WEB_PORT） | Web 界面（若走 HTTPS 反代可不对公网开放） |
| **UDP 52000–52100** | WebRTC 画面推流，**漏了它就是能登录但黑屏** |

## 配 HTTPS（推荐）

裸 HTTP 登录密码是明文的，长期用请套 HTTPS，二选一：

- 已有 nginx：用目录里的 `nginx-site.conf`（改域名和证书路径后启用）
- 用 Caddy 且 80/443 空闲：用 `Caddyfile.snippet`

配好后把 `.env` 的 `WEB_BIND` 改为 `127.0.0.1`，重跑 `bash deploy.sh`（界面只经反代暴露）。

## 常用调整（改 .env 后重跑 deploy.sh 生效）

| 需求 | 改动 |
|---|---|
| 改密码 | `NEKO_USER_PASSWORD` / `NEKO_ADMIN_PASSWORD` |
| 手机竖屏使用 | `NEKO_SCREEN=720x1280@30` |
| 画面卡顿省带宽 | `NEKO_SCREEN=1024x576@25` |
| 换界面端口 | `WEB_PORT` |

浏览器资料（登录态、Cookie、书签）在 `./profile/` 里持久化，备份这个目录就是备份浏览器；
删掉它等于恢复出厂。

## 更新 / 停止 / 卸载

```bash
bash deploy.sh          # 更新（拉最新镜像重启，数据保留）
docker compose down     # 停止
docker compose down && rm -rf profile .env   # 彻底卸载（含数据）
```

## 故障排查

| 现象 | 原因与解法 |
|---|---|
| 页面打不开 | `docker compose ps` 看容器状态；`docker compose logs -f neko` 看日志；确认 TCP 端口在安全组放行 |
| 能登录但黑屏/转圈 | 十有八九是 UDP 52000–52100 没在**云安全组**放行；或 `.env` 的 `PUBLIC_IP` 不是这台 VPS 的公网 IP（换过 IP 要改） |
| 画面糊/延迟高 | 降分辨率/帧率（见上表）；VPS 到你本地的线路质量决定下限 |
| 某网站提示"检测到代理/VPN"或给 410 | 是 VPS 出口 IP 被该站风控，与本方案无关——换出口 IP 才能解 |
| Chromium 标签页崩溃 | 内存不足：加内存，或确认 compose 里 `shm_size: 2gb` 没被改小 |

## 安全提示

- 这是单人/熟人自用工具：管理员密码不要外传，访客密码和管理员密码分开用。
- 不要把无密码状态暴露在公网；deploy.sh 默认生成 20 位随机密码，别改弱。
- 浏览器里登录过的账号都在 profile 里，把 VPS 和 profile 目录当敏感数据看管。
