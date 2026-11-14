# fzu-checkin · Go 服务使用说明

> 单二进制常驻服务：内建调度器（不再依赖 GitHub Actions cron）+ REST API + 原生前端。
> 设计与契约见 [go-service-design.md](go-service-design.md)。

## 1. 构建与启动

```bash
cd server
go build -trimpath -ldflags="-s -w" -o fzucheckin ./cmd/fzucheckin      # 生产二进制约 6.3 MB
./fzucheckin -listen 127.0.0.1:8787 -data ./data
```

Windows：

```powershell
cd server
go build -trimpath -ldflags="-s -w" -o fzucheckin.exe ./cmd/fzucheckin
.\fzucheckin.exe -listen 127.0.0.1:8787 -data .\data
```

参数与环境变量：

| 参数 | 环境变量 | 默认 |
|---|---|---|
| `-listen` | `FZU_LISTEN` | `127.0.0.1:8787` |
| `-data` | `FZU_DATA_DIR` | `./data` |
| `-trust-proxy` | `FZU_TRUST_PROXY=1` | 关 |
| `-version` | — | 打印版本后退出 |

- `FZU_LOG_LEVEL=debug|info|warn|error` 控制日志级别。
- `-trust-proxy` 只在**服务确实躲在可信反向代理后面**时才开：开着意味着登录限速的身份取自 `X-Forwarded-For`，而那是客户端能随手写的头（默认关，用 TCP 来源地址，防止换头绕过限速）。
- **数据目录只由 `-data` / `FZU_DATA_DIR` 决定**（`config.json` 就在数据目录里，读它之前得先知道目录）。进程启动时会把解析后的绝对路径写回配置，界面显示的就是真值。

首次启动会在 `data/` 下生成配置、Session 密钥与密码文件（权限 0600）。

## 2. 三步用起来

1. 浏览器打开 <http://127.0.0.1:8787> → 设置管理密码（≥8 位，只此一次）。
2. 「配置」页填：学号 + 密码（或免登 token）、宿舍楼经纬度（[高德坐标拾取器](https://lbs.amap.com/tools/picker)）、推送渠道（可选）→ 保存。
   - 已有旧版 `config.yaml`：点「导入 YAML」粘贴全文 → 预览 → 应用。
3. 「仪表盘」看今日状态与下一档倒计时；到点自动签到，结果进「历史」与推送。

## 3. 界面

| 视图 | 作用 |
|---|---|
| 仪表盘 `#/dashboard` | 今日是否已签、窗口内/外、今日计划与迟到线、下一档倒计时、时间轴三态（待触发/已完成/已错过）、`立即签到` / `定位预检` / `推送测试`、最近 5 次运行 |
| 配置 `#/config` | 账号 / 位置（单点或位置池）/ 时间调度 / 假期 / 推送 / 服务参数；带行内校验与警告；导入 YAML、导出 |
| 日志 `#/logs` | 历史日志快照 + SSE 实时流（等价 `tail -f`），级别过滤、自动滚动 |
| 历史 `#/runs` | 运行记录分页与详情抽屉（含坐标、校区、迟到标记、错误原文） |

## 4. 常用 API（全部 `/api/v1`，非 GET 需带 `X-Requested-With: fzu-web`）

```bash
# 健康检查（无需鉴权）
curl -s http://127.0.0.1:8787/api/v1/health

# 登录（拿到 fzu_sess Cookie 后其余接口都带上）
curl -s -c jar -H 'X-Requested-With: fzu-web' -H 'Content-Type: application/json' \
     -d '{"password":"你的管理密码"}' http://127.0.0.1:8787/api/v1/auth/login

# 今日状态 / 下一档 / 决策
curl -s -b jar http://127.0.0.1:8787/api/v1/status

# 手动补签（跳过等待与窗口拦截）
curl -s -b jar -H 'X-Requested-With: fzu-web' -H 'Content-Type: application/json' \
     -d '{"mode":"force"}' http://127.0.0.1:8787/api/v1/runs

# 定位预检（只 init + check，不打卡）
curl -s -b jar -H 'X-Requested-With: fzu-web' -H 'Content-Type: application/json' \
     -d '{"mode":"normal","dry_run":true}' http://127.0.0.1:8787/api/v1/runs

# 实时日志
curl -N -b jar http://127.0.0.1:8787/api/v1/logs/stream
```

完整契约（含错误码与 JSON 结构）见设计文档 §9。

## 5. 与 Python 版的行为对齐

| 项目 | 说明 |
|---|---|
| 时间基准 | 全部按北京时间（UTC+8）计算，与宿主机时区无关 |
| 调度 | 窗口 21:00–23:59、多档 `times`、时间抖动 ±N 分钟（越界重抽 8 次→贴边→执行前二次校验）、`max_wait`、`tolerance`、六条决策分支与 Python 逐条等价（323 例跨语言一致性语料） |
| 补签 | 启动时若「今天在窗口内 + 未签到 + 已过最后一档」自动补一次；界面「立即签到」= `CHECKIN_FORCE` |
| 位置 | 单点或位置池加权随机；以选中点为圆心半径内**按面积均匀**取点，`check` 与 `clockIn` 用同一个点，越界重取 ≤3 次后回退基准点 |
| 假期 | 区间内不签、不推、不留记录（`vacation.notify=true` 才推提醒） |
| 推送 | Server酱 / PushPlus / Bark / 企业微信；同一天同一状态只推一次，推送失败不影响签到结论 |
| 幂等 | 三道闸：`last_slot_key` → 今日已签 → 学校接口 `FState` |

## 6. 测试与验收

```bash
cd server
go vet ./... && gofmt -l .        # 静态检查（应无输出）
go test -count=1 ./...            # 全部离线：假学校上游、临时数据目录
go test -count=1 -cover ./...     # 覆盖率：schedule 97.5% / fzu 90.6% / config 89.8% / store 89.6% / server 79.7%
python ../scripts/gen_parity.py   # 重新生成 Python↔Go 一致性语料（323 例）+ AES 向量
```

- **跨语言一致性**：`internal/schedule/parity_test.go` 重放 Python 版生成的 323 例决策语料（含六条分支、抖动钳制、边界秒）。
- **加密字节级对齐**：`internal/secure/secure_test.go` 用 Python 生成的密文/明文向量断言 AES-CBC 与 AES-ECB 输出逐字节相同（含中文 `\uXXXX` 转义）。
- **竞态检测**：本机默认没有 gcc，`go test -race` 需要 MinGW；也可用 Clang/MSVC 包装器（见下）。
- **端到端冒烟**：`pwsh -File scripts/smoke.ps1`（构建 → 启动 → 初始化 → 配置校验 → 脱敏 → SSE → 资源占用 → 优雅退出）。
- **浏览器验收**：`pwsh -File scripts/ui-verify.ps1`（Chrome + CDP 跑四视图并截图到 `docs/verify-shots/`）。

```powershell
# 本机没有 MinGW 时用 Clang/MSVC 跑 -race（需 VS Build Tools）
call "…\VC\Auxiliary\Build\vcvars64.bat"
$env:CGO_ENABLED=1; $env:CC="<clang 包装器>"; $env:CGO_LDFLAGS="-lSynchronization"
go test -race -ldflags="-linkmode=external -extldflags=-Wl,/base:0x400000" ./...
```

## 7. 部署

**systemd（Linux）**

```ini
[Unit]
Description=fzu-checkin
After=network-online.target

[Service]
ExecStart=/usr/local/bin/fzucheckin -listen 127.0.0.1:8787 -data /var/lib/fzucheckin
Restart=on-failure
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/fzucheckin
DynamicUser=yes

[Install]
WantedBy=multi-user.target
```

**Windows 开机自启**

```powershell
schtasks /create /tn fzucheckin /tr "C:\path\fzucheckin.exe -listen 127.0.0.1:8787 -data C:\path\data" /sc onstart /ru SYSTEM /f
```

**反向代理（nginx）**：`proxy_buffering off;` 供 SSE，`proxy_read_timeout 300s;`，TLS 在代理层终结，服务只监听 127.0.0.1。

## 8. 资源占用（本机实测）

| 指标 | 实测 | 预算 |
|---|---|---|
| 空闲 RSS | 10.8 MB | ≤ 30 MB |
| 二进制（`-s -w -trimpath`） | 6.28 MB | ≤ 12 MB |
| goroutine 数 | 8–10 | — |
| 冷启动到可服务 | < 200 ms | ≤ 200 ms |

## 9. 安全须知

- 首次启动必须设置管理密码；密码以 PBKDF2-HMAC-SHA256（12 万轮）存储，Session 为 HMAC 签名 Cookie（7 天）。
- 登录失败同 IP 15 分钟 5 次触发限速（429）；**限速身份默认取 TCP 来源地址**，只有 `-trust-proxy` 时才采信 `X-Forwarded-For`（否则换一个头就能绕过）。
- 所有非 GET 请求需带 `X-Requested-With: fzu-web`（CSRF）；API 响应统一 `Cache-Control: no-store`，CSP 只加在自有页面上。
- 配置接口只回显 `••••` 哨兵；导出为脱敏版本；日志与 SSE 不打印 token/密码/推送 key。
- **「定位预检」不会产生签到记录、不会推送、不会把当天标成已签**——它是只读探测，可以放心在白天用。
- `data/` 含凭据与密钥，已在 `.gitignore` 中排除，切勿提交或共享。

## 10. 常见问题

| 现象 | 原因与处理 |
|---|---|
| 打开页面只看到「请通过服务访问」 | 直接双击了 `index.html`（`file://`）——必须通过服务 URL 访问 |
| 登录后立刻回登录页 | Cookie 被浏览器拦：确认用 `http://127.0.0.1:8787` 而不是 `localhost` 混用，或反向代理透传了 `X-Forwarded-Proto: https` 但实际是 http |
| 窗口外启动什么都不做 | 设计行为：窗口外一律静默跳过；要立刻验证用「立即签到」 |
| 服务器暂未返回今日计划 | 跨午夜 0:00–1:00 的 500，属正常；服务会重试 |
| 签名/时间不对 | 服务内部一律按北京时间判定，宿主机时区不影响结果 |
