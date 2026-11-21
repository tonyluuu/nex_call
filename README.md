# NexCall：香橙派 Zero2 与浏览器实时语音通话

基于 WebRTC 的全双工语音通话。腾讯云 VPS 上运行 LiveKit（SFU，内置 TURN）、业务服务 call-server 和 Caddy；香橙派 Zero2 运行 device-agent；浏览器和手机直接打开网页通话。服务端只用公网 IP + HTTPS，不需要域名。

```
nex_call/
  server/   call-server（Go）：登录、设备注册、呼叫信令、LiveKit Token、SQLite 通话记录
  device/   device-agent（Go，arm64）：PipeWire 或 PulseAudio 音频 + Opus + LiveKit，按键/LED，断线重连
  web/      网页端（Vite + TypeScript + livekit-client）
  deploy/   docker-compose、Caddyfile、livekit.yaml、IP 证书脚本；device/ 下是香橙派的配置和脚本
```

## 一、服务端部署（腾讯云 VPS）

1. 准备：Docker + Docker Compose；绑定弹性公网 IP（EIP）。
2. 安全组放通：TCP 80、443、7881；UDP 7882、3478。7880、8080 不要对公网开放。
   如果 LiveKit 日志显示有客户端走 TURN 中继，再放通 UDP 30000-30100。
3. 内核参数（LiveKit 推荐）：
   ```bash
   echo 'net.core.rmem_max=5000000' | sudo tee /etc/sysctl.d/60-livekit.conf && sudo sysctl --system
   ```
4. 拷贝仓库到 `/opt/nex_call`，构建网页（在 VPS 或本地均可，产物在 `web/dist`）：
   ```bash
   cd web && npm ci && npm run build
   ```
5. 配置并签发证书：
   ```bash
   cd deploy
   cp .env.example .env    # 填写 PUBLIC_IP、ACME_EMAIL、LIVEKIT_API_KEY/SECRET、SESSION_SECRET
   ./cert-renew.sh         # 先放自签名占位证书，启动 Caddy，再通过 HTTP-01 签发 Let's Encrypt IP 证书
   docker compose up -d --build
   ```
   `ACME_EMAIL` 必须是真实邮箱，不能用 `example.com`。签不到 IP 证书时执行 `./cert-renew.sh --self-signed` 兜底（浏览器首次访问需手动信任）。
   80/443 必须空闲给 Caddy：本机已有宝塔 Nginx 时见「六、服务端部署排错」。
6. 证书约 6 天有效，加定时续期：
   ```bash
   echo '17 */12 * * * /opt/nex_call/deploy/cert-renew.sh >> /var/log/nexcall-cert.log 2>&1' | crontab -
   ```
7. 添加网页用户和设备：
   ```bash
   docker compose exec call-server call-server user add alice          # 交互输入密码
   docker compose exec call-server call-server user add admin --admin  # 管理员：可看全部通话、移出网页参与者
   docker compose exec call-server call-server device add pi1 客厅      # 输出 DEVICE_ID / DEVICE_SECRET，只显示一次
   ```
   已有用户改角色：`call-server user role alice admin`。
8. 浏览器访问 `https://<公网IP>/nexcall/`。登录后能看到设备、在线用户和进行中的通话。根路径 `/` 不会跳转。

网页、接口、信令和 LiveKit 都挂在 `/nexcall` 下。香橙派的 `SERVER_URL` 也要带这个前缀。

### 自动部署

首次部署仍按上面 1–8 步完成：VPS 上要先有 Docker、`/opt/nex_call/deploy/.env` 和证书，SSH 用户要能写 `/opt/nex_call` 并执行 `docker`。之后三条工作流按改动的目录分别运行，也可以在 Actions 页手动运行其中一条：

- `web`：`web/` 有变更时构建网页，只同步 `web/dist`。Caddy 直接读这个目录，不重编镜像、不重启容器。
- `server`：`server/` 或 `deploy/`（不含 `deploy/device/`）有变更时，把源码同步到 VPS（排除 `.env`、`certs/`、`acme/`、`lego/`），再在 VPS 上 `docker compose up -d --build`。镜像层缓存在 VPS 本地，依赖没变时只重编 Go 代码。
- `device`：见第二节。只打包二进制，不连接 VPS。

`web` 和 `server` 同时触发时会排队，避免两次部署一起写 VPS。两条都不修改 `.env`、证书、SQLite 数据卷，也不改安全组或续期 crontab。

仓库 Secrets（`web` 和 `server` 共用）：

- `VPS_HOST`：公网 IP
- `VPS_USER`：SSH 用户
- `VPS_SSH_KEY`：私钥全文（含 BEGIN/END 行，不能带口令）
- `VPS_SSH_PORT`：可选，默认 22

### 通话方式

- 每台 Pi 连上后一直待在自己的房间里，没有网页用户时麦克风静音，有人进入就开麦。网页点「进入」直接通话，Pi 不用接听。
- 网页呼叫另一个网页时，对方自动进入同一个临时房间；其他人可以在「进行中的通话」里加入。最后一名网页用户离开后，这次通话结束。
- 一个人同时只在一个房间里。登录后有三个标签：「通话」「通话记录」「管理」。管理员的记录标题是「通话记录（全部）」，可以展开每条记录，也能把网页参与者移出房间（不能移出 Pi）。普通用户看到的是自己的「通话记录」。
- 管理员在「管理」页新增用户时，密码至少 8 位，并得到一条免密登录链接。链接里是用户名和加密凭证，没有明文密码。有效天数默认 1 天，填 0 表示不限制，最长 3650 天。改密码或重新签发后，旧链接失效。无效链接会提示改用用户名和密码登录。
- 管理员可以在设备卡片上调节喇叭和麦克风（0–200%，超过 100% 是软件增益）。香橙派音乐页改的是同一路系统音量，大约一秒内同步到网页；网页调完也会写回香橙派。喇叭调节停手后，香橙派播放一声短促高音；拖动过程中不响，只调麦克风不响。

## 二、香橙派

系统是 arm64（`uname -m` 为 `aarch64`），接 USB 声卡或 USB 会议麦克风。`setup.sh` 会按系统选择音频栈，虚拟设备名都是 `echo-cancel-source` / `echo-cancel-sink`：

- Armbian / Debian bookworm：PipeWire。
- 已经在用的 Ubuntu 18.04（Orange Pi 官方 Bionic 镜像）：没有 PipeWire，改用 PulseAudio 11 的 WebRTC 回声消除。内核要在 3.2 以上，这套官方镜像一般是 4.9 或更新。

1. 下载部署包。在 Actions 的 `device` 运行里下载 artifact `nexcall-device-linux-arm64`，里面是 `nexcall-device-linux-arm64.tar.gz`。二进制按 Ubuntu 18.04（glibc 2.27）构建，bookworm 和 18.04 都能直接安装，并带上安装脚本和回声消除配置。手动运行该工作流，或 `device/`、`deploy/device/` 有变更并推送到 `main` 时自动打包。派上已有的 `device.env` 不会被覆盖。
   ```bash
   tar -xzf nexcall-device-linux-arm64.tar.gz
   cd nexcall-device
   ```
2. 初始化音频环境并自检：
   ```bash
   ./setup.sh && ./audio-test.sh
   ```
   Ubuntu 18.04 若 `apt-get update` 失败，把源里的 `ports.ubuntu.com` 换成 `old-releases.ubuntu.com`，路径仍是 `ubuntu-ports`。
3. 安装为 systemd 服务并填写配置：
   ```bash
   ./install.sh
   sudo nano /etc/nexcall/device.env     # SERVER_URL=https://<公网IP>/nexcall、DEVICE_ID、DEVICE_SECRET
   sudo systemctl restart device-agent && journalctl -u device-agent -f
   ```
   也可以不下载部署包，在源码的 `deploy/device/` 里执行同样的脚本。二进制另外准备：
   - 在电脑上打出同一份二进制：`cd device && docker buildx build --platform linux/arm64 -f Dockerfile.bionic -o type=local,dest=./dist .`，再 `./install.sh ../device/dist/device-agent`。
   - 在香橙派上直接编译（官方 Go 压缩包是静态链接的，18.04 能跑）：`export PATH=$PATH:/usr/local/go/bin` 后执行 `cd device && CGO_ENABLED=1 go build -tags nolibopusfile -o device-agent ./cmd/device-agent`。18.04 自带的 libopus 过旧，`setup.sh` 会另装 1.3.1。内存只有 1GB 时先加交换分区，否则编译会被杀掉：`sudo fallocate -l 2G /swapfile && sudo chmod 600 /swapfile && sudo mkswap /swapfile && sudo swapon /swapfile`

按键与指示灯（`BUTTON_LINE` / `LED_LINE` 用 `gpioinfo` 查线号；Ubuntu 18.04 源里通常没有这个命令，不用按键和灯时保持 `-1`）：

| 状态 | 短按 | LED |
| --- | --- | --- |
| 离线 | 无作用 | 慢闪 |
| 在房间待命（无人） | 提醒 `CALL_TARGET`（为空则所有在线网页） | 每 3 秒闪一下 |
| 有人在房间 | 切换静音 | 常亮；静音时快闪 |

没接按键时可以用信号模拟短按：`sudo systemctl kill -s USR1 device-agent`。

调试时可以跳过 call-server，直接用 Token 进房：
```bash
# 服务器上签一个 Token
docker compose exec call-server call-server token test-room device:pi1
# 香橙派上直连，-test-tone 用 440Hz 测试音代替麦克风
device-agent -livekit-url wss://<公网IP>/nexcall -token <TOKEN> [-test-tone]
```

### 香橙派音乐与通话设置（可选）

`music/` 是一个独立的 Flask 小服务，浏览器打开 `http://<香橙派IP>:8090`。页面分成两个标签：

- **音乐播放**：拖拽上传、删除 MP3，播放、暂停、拖动进度、上一曲 / 下一曲（播完自动下一曲），并显示封面、标签和存储空间占用。声音走 `echo-cancel-sink`，通话时不会把音乐回传给网页。音乐在播放或暂停时，切到通话设置仍保留底部播放条；已经停止时隐藏。
- **通话设置**：来电提示音、喇叭和麦克风音量、麦克风电平、录音、降噪开关。来电提示可以关掉，或在六种短滴里选一种（一声短滴、滴滴两声、高音短促、低音一声、电话铃、三声短滴）。喇叭滑块松开时播放「高音短促」。麦克风电平看的是 USB 声卡送来的声音，还没进降噪。录音最长 3 分钟，录的是通话用的单声道，通话中不能录。降噪只有开关，没有强度。

```bash
cd music && ./install.sh     # 安装 mpg123，复制到 /opt/nexcall-music，注册 nexcall-music 服务
```

曲目和设置放在 `/var/lib/nexcall-music`。

- device-agent 把通话状态写到 `/run/nexcall/call-state`（`CALL_STATE_FILE`）。有网页用户进房时播放器自动暂停，挂断后不会自动继续。接通前向本机 `MUSIC_URL`（默认 `http://127.0.0.1:8090`）请求所选短滴；提示关掉时直接进房。
- 音乐页和网页管理改的是同一路 PulseAudio 音量。device-agent 在线时大约每秒看一次，有变化就上报给网页。
- 没有登录保护，只在局域网使用，不要把 8090 端口映射到公网。

## 三、本地开发

需要 Go 1.26+、Node 22+、Docker。系统 Go 较旧时在 `server/`、`device/` 目录下执行命令会自动切换工具链（仓库根目录的 `go.work` 需要 Go 1.26 才能直接用）。

```bash
# LiveKit 开发服务器（密钥 devkey / secret）
docker run -d --name lk-dev -p 7880:7880 -p 7881:7881 -p 7882:7882/udp \
  livekit/livekit-server --dev --bind 0.0.0.0 --node-ip 127.0.0.1

# call-server
cd server
export LIVEKIT_URL=ws://localhost:7880 LIVEKIT_API_KEY=devkey LIVEKIT_API_SECRET=secret \
       SESSION_SECRET=local-dev-session-secret WEB_DIR=../web/dist
go run ./cmd/call-server user add alice password123
go run ./cmd/call-server device add pi1 测试设备
go run ./cmd/call-server serve

# 网页：npm run dev 后打开 http://localhost:5173/nexcall/（Vite 把 /nexcall/api 和 /nexcall/ws 代理到 8080）
cd web && npm install && npm run dev
```

测试：`cd server && go test -race ./...`；设备端的纯 Go 部分 `cd device && go test ./internal/jitter ./internal/playout ./internal/agent ./internal/gpio`，全部测试（含 cgo）在 `device/Dockerfile` 构建时执行。

## 四、调优参数与实测

device-agent 每 10 秒打印一次 `call stats`：收发包数、丢包、FEC 恢复、PLC、迟到、欠载、抖动缓冲当前深度和目标深度。

抖动缓冲是自适应的：初始目标深度为 `JITTER_TARGET_MS`（默认 60ms），出现迟到包就加深一帧，最多加到 `JITTER_MAX_MS` 的一半；稳定 10 秒后逐步回落；积压过深时丢弃最旧的帧以控制延迟。

用 `tc netem` 在本地实测（两台 device-agent 经 LiveKit 互通，每个方向每秒 50 包）：

| 网络条件 | 结果 |
| --- | --- |
| 无损伤 | 丢包、迟到、欠载均为 0，目标深度保持 60ms |
| 上下行各丢 10%、延迟 30±20ms | 约 81% 的丢包由 Opus 带内 FEC 恢复，其余 PLC 补偿；欠载 0～1 次，目标深度不变 |
| 上下行各延迟 50±40ms | 目标深度自适应到 140ms，迟到率约 3%（固定 60ms 时约 7%） |

单向延迟大致由这几部分组成：声卡缓冲（`AUDIO_PERIOD_MS` × 3）+ 20ms 编码帧 + 网络 + 抖动缓冲（60ms 起）。

回声消除只能在真机上调：先跑 `deploy/device/audio-test.sh` 对比原始和消回声后的录音；效果不好时降低喇叭音量、拉开喇叭与麦克风距离，或换用带硬件回声消除的 USB 会议麦克风。

## 五、常见问题

- 网页提示"不是 HTTPS"：浏览器只在 HTTPS 或 localhost 下开放麦克风，必须通过 `https://<公网IP>/nexcall/` 访问。
- 从旧版升级：推送到 `main` 后，`web/` 的变更只更新网页，`server/` 或 `deploy/`（不含 `deploy/device/`）的变更才重建 call-server。香橙派下载 `device` 工作流的 `nexcall-device-linux-arm64`（Ubuntu 18.04 和 bookworm 都能用），解压后执行 `./install.sh`。只换二进制、脚本没变时也可以 `sudo systemctl restart device-agent`。`SERVER_URL` 须为 `https://<公网IP>/nexcall`。需要管理员时执行 `call-server user role <用户名> admin`。旧的一对一振铃呼叫不再使用。
- 能登录但通话无声：检查安全组是否放通 UDP 7882 / TCP 7881，以及 LiveKit 日志里的 `nodeIP` 是否为公网 IP。
- 设备一直重连并提示 `unauthorized`：`DEVICE_SECRET` 不对，重新执行 `call-server device add` 生成新密钥。
- 自签名证书兜底时：把服务器的 `deploy/certs/fullchain.pem` 拷到香橙派，在 `device.env` 里设置 `CA_CERT`。

## 六、服务端部署排错

腾讯云 VPS（含宝塔）首次部署时踩过的问题。`server/Dockerfile` 的 `GOPROXY`、`cert-renew.sh` 的 `/.lego` 和 `--disable-cn` 已写进仓库，按当前脚本部署即可。

### 宝塔 Nginx 占用 80/443

Caddy 必须独占 TCP 80（HTTP-01）和 443（HTTPS）。宝塔的 Nginx 路径是 `/www/server/nginx/`，**不是** systemd 服务，所以 `systemctl stop nginx` 不会停掉它：

```bash
ss -lntup | grep -E ':80|:443'
ps -ef | grep '[n]ginx'
# master 若为 /www/server/nginx/sbin/nginx，用宝塔自己的方式停：
/etc/init.d/nginx stop
# 或 /www/server/nginx/sbin/nginx -s stop
# 并在宝塔面板里停止 Nginx，防止开机再拉起
```

这台机上还有别的网站时不要抢端口，应改成 Nginx 反代，或换一台只跑 NexCall 的机器。停干净后应看到 `docker-proxy` 在听 80/443，而不是 `nginx`。

### `./cert-renew.sh` 卡在构建 call-server

脚本为了起 Caddy 会 `docker compose up -d caddy`，Caddy 依赖 call-server，于是先编镜像。国内 VPS 访问 `proxy.golang.org` 经常极慢或一直挂起，日志停在 `RUN go mod download` 十几分钟，网卡 `rxkB/s` 只有约 1KB。Ctrl+C 后确认 `server/Dockerfile` 里有 `ENV GOPROXY=https://goproxy.cn,direct`，再 `docker compose build call-server`。BuildKit 构建时 `docker ps -a` 可能是空的，这是正常的。

### lego 报 `mount ... /lego: not a directory`

`goacme/lego` 镜像里 `/lego` 是入口二进制，不是目录。数据目录必须挂到 `/.lego`，并用 `--path /.lego`。可自检：

```bash
docker run --rm --entrypoint ls goacme/lego:v4.28.1 -l /lego
# 应看到一个约 64MB 的可执行文件
```

### `contact email has forbidden domain "example.com"`

Let's Encrypt 拒收 `example.com` 邮箱。改 `deploy/.env` 里的 `ACME_EMAIL` 为真实邮箱后再跑脚本。`.env.example` 里的地址只是占位。

### `CSR contains IP address in Common Name`

HTTP-01 已经通过、卡在 finalize：Let's Encrypt 不允许把 IP 写进 CSR 的 CN，IP 只能在 SAN。`lego run/renew` 必须带 `--disable-cn`（v4.25+）。

### `.env` 各项用途

签发证书真正用到的是 `PUBLIC_IP`（证书 SAN / Caddy 站点名 / LiveKit `--node-ip`）和 `ACME_EMAIL`。`LIVEKIT_API_KEY`、`LIVEKIT_API_SECRET`、`SESSION_SECRET` 给 `docker compose` 用，和 ACME 无关，但写在同一份 `.env` 里，部署时一起填。
