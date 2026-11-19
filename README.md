# NexCall：香橙派 Zero2 与浏览器实时语音通话

基于 WebRTC 的全双工语音通话。腾讯云 VPS 上运行 LiveKit（SFU，内置 TURN）、业务服务 call-server 和 Caddy；香橙派 Zero2 运行 device-agent；浏览器和手机直接打开网页通话。服务端只用公网 IP + HTTPS，不需要域名。

```
nex_call/
  server/   call-server（Go）：登录、设备注册、呼叫信令、LiveKit Token、SQLite 通话记录
  device/   device-agent（Go，arm64）：PipeWire 音频 + Opus + LiveKit，按键/LED，断线重连
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
   签不到 IP 证书时执行 `./cert-renew.sh --self-signed` 兜底（浏览器首次访问需手动信任）。
6. 证书约 6 天有效，加定时续期：
   ```bash
   echo '17 */12 * * * /opt/nex_call/deploy/cert-renew.sh >> /var/log/nexcall-cert.log 2>&1' | crontab -
   ```
7. 添加网页用户和设备：
   ```bash
   docker compose exec call-server call-server user add alice          # 交互输入密码
   docker compose exec call-server call-server device add pi1 客厅      # 输出 DEVICE_ID / DEVICE_SECRET，只显示一次
   ```
8. 浏览器访问 `https://<公网IP>`，登录后能看到设备列表。

## 二、香橙派 Zero2

1. 刷 Armbian（Debian bookworm），接 USB 声卡或 USB 会议麦克风。
2. 初始化音频环境（PipeWire + WebRTC 回声消除），并自检：
   ```bash
   cd deploy/device && ./setup.sh && ./audio-test.sh
   ```
3. 编译 device-agent（任选其一）：
   - 在电脑上用 Docker 交叉编译：`cd device && docker buildx build --platform linux/arm64 -o type=local,dest=./dist .`，然后把 `dist/device-agent` 拷到香橙派。
   - 在香橙派上直接编译：`cd device && CGO_ENABLED=1 go build -tags nolibopusfile -o device-agent ./cmd/device-agent`
4. 安装为 systemd 服务并填写配置：
   ```bash
   ./install.sh ./device-agent
   sudo nano /etc/nexcall/device.env     # SERVER_URL=https://<公网IP>、DEVICE_ID、DEVICE_SECRET
   sudo systemctl restart device-agent && journalctl -u device-agent -f
   ```

按键与指示灯（`BUTTON_LINE` / `LED_LINE` 用 `gpioinfo` 查线号）：

| 状态 | 短按 | 长按 | LED |
| --- | --- | --- | --- |
| 待机 | 呼叫 `CALL_TARGET`（为空则所有在线用户） | - | 每 3 秒闪一下（离线时慢闪） |
| 来电 | 接听 | 拒接 | 快闪 |
| 呼出 / 通话中 | 挂断 | 挂断 | 中速闪 / 常亮 |

没接按键时可以用信号模拟：`sudo systemctl kill -s USR1 device-agent`（短按）、`-s USR2`（长按）。

调试时可以跳过 call-server，直接用 Token 进房：
```bash
# 服务器上签一个 Token
docker compose exec call-server call-server token test-room device:pi1
# 香橙派上直连，-test-tone 用 440Hz 测试音代替麦克风
device-agent -livekit-url wss://<公网IP> -token <TOKEN> [-test-tone]
```

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

# 网页：npm run dev（通过 Vite 代理访问 8080），或 npm run build 后直接打开 http://localhost:8080
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

- 网页提示"不是 HTTPS"：浏览器只在 HTTPS 或 localhost 下开放麦克风，必须通过 `https://<公网IP>` 访问。
- 能登录但通话无声：检查安全组是否放通 UDP 7882 / TCP 7881，以及 LiveKit 日志里的 `nodeIP` 是否为公网 IP。
- 设备一直重连并提示 `unauthorized`：`DEVICE_SECRET` 不对，重新执行 `call-server device add` 生成新密钥。
- 自签名证书兜底时：把服务器的 `deploy/certs/fullchain.pem` 拷到香橙派，在 `device.env` 里设置 `CA_CERT`。
