# browser-box

跑在自己服务器上的远程浏览器：浏览器本体在服务器上运行，画面推到你的手机/电脑，你的点击和打字再送回去。所有网页流量从服务器出口走。

**自研实现**（v2）：单个 Go 程序通过 CDP 协议直接驱动官方独立版 Chrome（Chrome for Testing），把画面以帧流经 WebSocket 推给前端。不用 Docker、不拉镜像、不用 WebRTC/UDP、不用填公网 IP——**只占一个 TCP 端口**。登录只有密码，没有用户名环节。浏览器崩了会自动拉起，资料（Cookie/登录态）存在 `profile/` 里，重启不丢。

> v1 是基于 Neko 的 Docker 部署包，已被本实现取代；deploy.sh 会自动停用旧的 Neko 容器。

## 一键部署

前置：一台能联网的 Linux 服务器（Debian/Ubuntu，x86_64），root 或 sudo。

```bash
curl -fsSL https://raw.githubusercontent.com/cesar699/browser-box/main/deploy.sh | sudo bash
```

脚本做的事：克隆本仓库到 `/opt/browser-box` → 装 Go 工具链 → 下载 Chrome for Testing（版本锁定 156.0.8078.47）→ 编译 → 生成随机登录密码写入 `browser-box.env` → 注册 systemd 服务 `browser-box` 并启动 → 健康检查后打印访问地址和密码。

重复执行同一命令 = 更新（拉最新代码重编译重启，密码和浏览器资料保留）。

云安全组只需放行 **TCP 8080**（可用 `PORT=xxxx` 环境变量改端口）。

## 使用

打开 `http://<服务器IP>:8080`，输密码进入（**没有用户名**）。顶部地址栏输入网址回车打开；「粘贴」按钮可以把一段文字送进远程浏览器当前输入框（手机上打长串网址/密码时方便）。

- 改密码：编辑 `/opt/browser-box/browser-box.env`，然后 `systemctl restart browser-box`
- 看日志：`journalctl -u browser-box -f`
- 绑域名上 HTTPS：用 nginx/Caddy 反代到 `127.0.0.1:8080` 即可（需透传 WebSocket Upgrade），再把服务改成只听本机：`browser-boxd -listen 127.0.0.1:8080`（改 systemd 的 ExecStart）

## 本地运行（开发）

```bash
go build -o browser-boxd .
BROWSERBOX_PASSWORD=你的密码 ./browser-boxd -listen :8080
# Chrome 放在 ./chrome-linux64/chrome，或用 BROWSERBOX_CHROME 指定路径
```

## 已知边界

- v2 暂无声音（CDP 帧流只含画面与输入）；视频播放流畅度取决于服务器上行与帧率，日常浏览足够。
- 单页签：远程里点开的新窗口链接会被改成当前页跳转（远程屏幕没有第二个窗口可放）。
- 安全定位：个人自用。密码登录 + HttpOnly 会话 Cookie，公网长期使用请套 HTTPS。

## 本地验证记录（2026-10-23）

开发环境实跑通过：密码登录（对 200 / 错 401）、画面帧流连续且逐帧变化、点击坐标像素级准确（发送 0.25,0.12 → 页面收到 320,86 @1280×720）、键盘输入回环（输入框打出 "hi" 并触发页面事件）、视口 resize 后帧尺寸随之变化（1280×720 → 900×600）。期间修掉一个致命 bug：CDP 事件回调里同步等 ack 响应会把读循环堵死（改为事件队列 + ack 免等待发送）。
