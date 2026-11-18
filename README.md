<div align="center">

<img src="docs/assets/logo.png" width="96" height="96" alt="fzu-auto-checkin logo">

# fzu-auto-checkin

**福州大学「智汇福大」晚点名 · 自动签到**

一个二进制常驻面板，或一段 GitHub Actions 脚本：到点自动打卡，结果推到你微信。

[![License: MIT](https://img.shields.io/badge/license-MIT-2f6fd0.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.22%2B-00add8.svg)](server/go.mod)
[![Python](https://img.shields.io/badge/python-3.11%2B-3776ab.svg)](requirements.txt)
[![Version](https://img.shields.io/badge/version-1.6.6-1f7a4d.svg)](server/internal/server/server.go)

</div>

> ⚠️ **使用须知**
>
> 晚点名是学校的安全确认制度。本项目只用于**本人确实在校时防止漏签**：不伪造定位、不隐瞒真实在/离校状态、不接受付费代挂。违规使用的后果由使用者自行承担。本项目可能由于某些不可抗力、或者其他因素干扰随时下线，且用且珍惜。

> 📌 早期海报与宣传卡里的二维码指向一个已下线的在线配置页。**离线可用的那一份在 [`web/configurator.html`](web/configurator.html)**：下载后双击即用，纯前端、不联网保存、不经过任何服务器。

![宣传图](docs/assets/promo.png)

---

## 目录

- [两条跑法：怎么选](#两条跑法怎么选)
- [跑法 A：自建 Go 面板（推荐）](#跑法-a自建-go-面板推荐)
- [跑法 B：GitHub Actions + Python](#跑法-bgithub-actions--python)
- [功能一览](#功能一览)
- [时间调度](#时间调度)
- [位置与上报文案](#位置与上报文案)
- [假期跳过](#假期跳过)
- [结果推送](#结果推送)
- [面板 API](#面板-api)
- [一次签到做了什么](#一次签到做了什么)
- [数据与安全](#数据与安全)
- [项目结构](#项目结构)
- [构建与测试](#构建与测试)
- [仓库里不放什么](#仓库里不放什么)
- [已知限制](#已知限制)
- [常见问题](#常见问题)
- [贡献与反馈](#贡献与反馈)

## 两条跑法：怎么选

仓库里是同一套业务逻辑的两个实现：**Go 常驻服务**（`server/`，2026-11 起的主线，带 Web 面板）与**Python CLI**（`main.py` + `src/`，给 GitHub Actions 用的零服务器方案）。时间语义、位置解析、打卡链路、推送行为在两边是逐条对齐的（调度层有 323 例跨语言一致性语料）。

| | **Go 面板** | **GitHub Actions + Python** |
|---|---|---|
| 跑在哪 | 自己的电脑 / 小主机 / 服务器，常驻 | GitHub 托管的 runner，按 cron 唤起 |
| 有没有界面 | 浏览器面板：仪表盘 / 配置 / 日志 / 历史 | 没有界面，结论靠推送 |
| 调度 | 服务内建调度器，窗口内每 30 秒复查，启动即补签 | GitHub cron（最小 10 分钟）+ 脚本内等待到点 |
| 凭据存放 | `data/config.json` 里 AES-256-GCM 密文，密钥在同目录 `secret.key` | GitHub Secret 或仓库里的 `config.yaml`（**明文**，靠仓库私有与 Secret 兜底） |
| 依赖 | 一个二进制（Go 1.22 构建，**零第三方库**） | Python 3.11+ / requests / PyYAML / pycryptodome |
| 额外能力 | 一键启停、定位预检、面板内收短信验证码、SSE 实时日志、运行历史、YAML 导入导出 | fork 即用，手机零安装 |
| 代价 | 得有台常开的机器（或每天开机时段） | cron 不保证准点（实测延迟 1–4 小时），凭据在 Secret 里 |

两条路可以同时开：面板负责日常，Actions 当兜底。任意一边签成功后，另一边读到学校的已签状态会直接静默退出；想彻底避免两边同时触发，把面板的「自动签到」开关暂停、只留 Actions 即可。

## 跑法 A：自建 Go 面板（推荐）

### 一键启动（Windows）

双击仓库根的 `start.cmd`，或者在 PowerShell 里：

```powershell
.\start.ps1                      # 需要时自动 go build，然后前台跑起来，默认 http://127.0.0.1:14514
.\start.ps1 -Status              # 只探活（没在跑退出码 1）
.\start.ps1 -Port 14515 -NoBuild # 换端口、跳过构建
.\start.ps1 -Listen 0.0.0.0:14514 # 允许局域网访问（会打印本机 IP 的 URL）
```

脚本做的事只有四件：定位仓库 → 按需构建 `server\fzucheckin.exe` → 前台运行 → 等 `/api/v1/health` 就绪后按需打开浏览器。默认数据目录是 `server\data`。

### 手动构建与启动

```bash
cd server
go build -trimpath -ldflags="-s -w" -o fzucheckin ./cmd/fzucheckin
./fzucheckin -listen 127.0.0.1:8787 -data ./data
```

```powershell
cd server
go build -trimpath -ldflags="-s -w" -o fzucheckin.exe ./cmd/fzucheckin
.\fzucheckin.exe -listen 127.0.0.1:8787 -data .\data
```

> 端口有两套默认值：**代码里的默认监听是 `127.0.0.1:8787`**（`config.DefaultListen`），而 `start.ps1` 固定用 `-listen 127.0.0.1:14514` 覆盖它。命令行参数只影响这一次进程，不会回写配置。

### 参数与环境变量

| 参数 | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `-listen` | `FZU_LISTEN` | 配置里的 `server.listen`，缺省 `127.0.0.1:8787` | 监听地址 |
| `-data` | `FZU_DATA_DIR` | `./data` | 数据目录（`config.json` 等都在里面，只能由启动参数决定） |
| `-trust-proxy` | `FZU_TRUST_PROXY=1` | 关 | 采信 `X-Forwarded-For`。**只有服务确实躲在可信反代后面才开**，否则换个头就绕过登录限速 |
| `-no-scheduler` | `FZU_NO_SCHEDULER=1` | 关 | 只开面板与手动操作，不跑自动签到（本地调试、截图用，避免调试顺手打一次真卡） |
| — | `FZU_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `-version` | — | — | 打印版本号后退出 |

### 三步用起来

1. 浏览器打开服务地址（`start.ps1` 是 <http://127.0.0.1:14514>）→ 设置管理密码（**≥ 8 位**，只此一次）。
2. 「配置」页填三块内容并保存：
   - **账号**：免登 token 直接填在卡片上；学号密码在弹窗里（「测试登录」只验证不写配置，「获取并保存凭证」会先跑一次学校端 `init` 校验，通过才落盘）。三种凭据优先级 **token > 浏览器 Cookie > 学号密码**。
   - **位置**：点「在地图上选点」直接在高德底图上选；或用[高德坐标拾取器](https://lbs.amap.com/tools/picker)量一个 GCJ-02 坐标。
   - **推送**（可选）：Server酱 / PushPlus / Bark / 企业微信群机器人，填一种即可。
3. 「仪表盘」看今日状态与下一档倒计时，剩下的到点自动完成。

不想手抄 token 的话：在晚点名页面按 F12 跑一行脚本，或存成书签，把地址栏里的 `token=` 直接交给面板——见 [docs/get-token.md](docs/get-token.md)。

已有旧版 `config.yaml`：配置页点「导入 YAML」→ 粘贴全文 → 预览 → 应用，会跑同一套规范化与校验（写坏的项降级成默认值并逐条提示）。

### 面板里有什么

| 视图 | 路由 | 关键内容 |
|---|---|---|
| 仪表盘 | `#/dashboard` | 今日是否已签、在/不在窗口、今日计划与迟到线、下一档倒计时、**自动签到总开关**、`立即签到` / `定位预检` / `推送测试`、最近 5 次运行 |
| 配置 | `#/config` | 账号 / 位置 / 时间调度 / 假期 / 推送 / 服务参数；行内校验与警告；导入 YAML、导出（脱敏） |
| 日志 | `#/logs` | 历史日志快照 + SSE 实时流（等价 `tail -f`），级别过滤、自动滚动 |
| 历史 | `#/runs` | `runs.jsonl` 分页与详情抽屉（坐标、校区、迟到标记、错误原文） |

界面是左右分栏，窄屏自动变顶部横向导航；控件全部自绘（不跟浏览器默认外观走）。页面无内联脚本/样式，CSP 收到 `script-src 'self'; style-src 'self'`。

### 校外 IP 的短信二次验证

统一身份认证在校外 / 机房 IP 上会强制短信验证：账号密码是对的、会话 Cookie 也有效，但 CAS 不发票，得先过手机上那 6 位数字。这不是本项目的故障——验证码只会发到你手机，服务端能替你做的是"提交那 6 位数字"，替不了"替你收短信"。

面板把这条路做进去了：

1. 「账号」→「打开登录弹窗」→ 填学号密码 →「测试登录」；
2. 学校判成要短信时，弹窗当场把验证码发到你手机，并显示打码手机号（`138****1234`）与学校原话；
3. 填那 6 位数字（填满自动提交），通过后自动保存凭证；
4. API 侧对应 `POST /api/v1/login/sms/{start,resend,verify,cancel}`；发码冷却按实测的 5 分钟算，同号重复点会复用还活着的那一条。

`POST /api/v1/login/test`、`/login/refresh` 在需要二次验证时回 **409**、`reason=second_auth`，而不是含糊的"登录失败"。

## 跑法 B：GitHub Actions + Python

### 1. 拿到代码

```bash
git clone https://github.com/Mahiroa/fzu-auto-checkin.git
cd fzu-auto-checkin
pip install -r requirements.txt
```

### 2. 填配置

```bash
cp config.example.yaml config.yaml
```

`config.yaml` 已被 `.gitignore` 忽略。**它是明文**：SSO 登录必须拿到密码原文，哈希在这里用不上，所以 `src/config.py` 每次加载会把文件权限收紧到 `0600`（POSIX；收不紧就打印警告）。

最小可用只需要 4 项：

```yaml
user:
  username: "你的学号"          # 填学号密码即可，脚本自己换 token
  password: "登录智汇福大 App 的密码"

checkin:
  longitude: "119.20463"       # 高德 GCJ-02
  latitude: "26.04920"
```

| 配置项 | 填什么 | 去哪拿 |
|---|---|---|
| `user.username` / `password` | 登录智汇福大 App 的学号密码（**推荐**） | 你平时登 App 用的那套 |
| `user.token` | 免登 token（与账密二选一，配了就不走 SSO） | [docs/get-token.md](docs/get-token.md) |
| `checkin.longitude` / `latitude` | 校区内常驻位置的 GCJ-02 坐标 | 面板里「在地图上选点」，或[高德坐标拾取器](https://lbs.amap.com/tools/picker) |
| `checkin.actual_location` | 上报的地址文案 | 格式见 [docs/address-format.md](docs/address-format.md) |
| `notify.*` | 推送渠道，填一种 | [Server酱](https://sct.ftqq.com) / [PushPlus](https://www.pushplus.plus) |

> `user.cookie`（`sso.fzu.edu.cn` 的 `SESSION`）**只有 Go 面板会读**：它能在 token 过期时免密换新票。Python 这条路只认 token 与学号密码，别把希望挂在 cookie 上。

### 3. 先本地验证一次

```bash
python main.py
```

看到「已签到 / 无需操作」或「签到成功 ✅」就说明配置对了。当前时间不在签到窗口内时会打印一句原因直接退出，**这是设计行为，不是报错**。想立刻试一次：

```bash
CHECKIN_FORCE=1 python main.py          # bash / macOS / Linux
$env:CHECKIN_FORCE = "1"; python main.py # Windows PowerShell
```

### 4. 挂到云端

1. Fork 本仓库，或新建一个**私有仓库**把代码推上去；
2. `Settings → Secrets and variables → Actions → New repository secret`，名称 `CONFIG_YAML`，值 = 本地 `config.yaml` 的**全文**；
3. 完事。`.github/workflows/checkin.yml` 每 10 分钟扫一次北京时间 21:00–23:59，到点自动签。

> 也可以直接提交 `config.yaml`：workflow 的逻辑是「有 `CONFIG_YAML` secret 就用它，否则用仓库里的 `config.yaml`」。走这条路记得 `git add -f config.yaml`（`.gitignore` 默认忽略它），并且**必须是私有仓库**。

Actions 侧两个细节：`concurrency` 让整条 workflow 串行（已经开始的一次签到必须跑完，宁可排队也不打断半路请求），`timeout-minutes: 60` 兜住排队与早到等待。手动补签：`Actions → checkin → Run workflow → 勾 force`。

## 功能一览

| | 能力 | Go 面板 | Actions | 说明 |
|---|---|:--:|:--:|---|
| ⏰ | 自定义签到时刻 | ✅ | ✅ | `schedule.times` 想写几档写几档，如 `["21:35","22:10"]`；被提前叫醒会自动等到最近一档 |
| 🎲 | 时间抖动 | ✅ | ✅ | 每档 ±N 分钟随机偏移，越界先重抽、再贴边、执行前二次校验，**落不到窗口外** |
| 📍 | 自定义位置 | ✅ | ✅ | 单点坐标继续可用；`locations` 位置池按权重随机，上报文案跟着点走 |
| 🗺️ | 地图选点 | ✅ | — | 内嵌**高德公开瓦片**（街道/卫星可切），免 key、不占付费额度；点出来就是 GCJ-02 |
| 🌐 | 位置抖动 | ✅ | ✅ | 以选中点为圆心、默认 200 米半径的**圆面积内均匀**取点，不再每晚报同一个经纬度 |
| 🔑 | 免维护认证 | ✅ | ✅ | 学号密码走 SSO 换 token 并回写；也可直接填 token |
| 🍪 | Cookie 登录 | ✅ | — | 填 `sso.fzu.edu.cn` 的 `SESSION`，token 过期时免密续票（面板 UI 自 v1.6.1 不再提供输入框，老配置里的那份仍被当作备用凭据） |
| 🔒 | 凭据不落明文 | ✅ | — | 面板上密码/token/Cookie 一律 `••••`，眼睛按钮要**再输一次管理密码**才逐项取回；落盘是 AES-256-GCM 密文 |
| ⏯️ | 一键启停 | ✅ | — | 暂停只停自动调度（含补签/重试），手动「立即签到」照旧；状态存 `state.json`，重启后还在 |
| 🫥 | 凭据可清空 | ✅ | — | 三项凭据全留空也能保存（只提醒"现在不会自动签到"），方便先配坐标与推送 |
| 🔗 | 凭据交接 | ✅ | — | 晚点名页面粘一行脚本，把地址栏 `token=` 直接交给面板（[docs/get-token.md](docs/get-token.md)） |
| 🎯 | 定位预检 | ✅ | — | 只 `init` + `check`，不提交打卡、不推送、不标今日已签；白天也能放心点 |
| 📲 | 结果推送 | ✅ | ✅ | Server酱 / PushPlus / Bark / 企业微信，也可完全静默 |
| 🌴 | 假期跳过 | ✅ | ✅ | 按校历预填寒暑假，也能自己加任意免签区间 |
| 🕘 | 手动补签 | ✅ | ✅ | 面板「立即签到」 / `mode=force`；Actions 勾 `force`；本地 `CHECKIN_FORCE=1` |
| 🧪 | 离线可测 | ✅ | ✅ | 时间与随机源全部可注入，不联网、不读真实配置 |
| 🤖 | 备选方案 | — | — | 附 v0.1 Auto.js 无障碍脚本（模拟点击，不碰接口），供参考 |

技术路线是**接口级**而非模拟点击：静态分析晚点名 H5 前端，逆向出 4 个接口与 AES 参数加密，再用 HTTP 完整复现。不用 Frida、不抓包也能在云端跑。

## 时间调度

`checkin.schedule` 整段都可以不写，缺省等价于「每天 21:35 签一次，窗口 21:00–23:59」。所有时刻一律按**北京时间（UTC+8）**解释，与本机时区、runner 的 UTC 都无关。

| 键 | 默认值 | 含义 |
|---|---|---|
| `window_start` | `"21:00"` | 允许动手的窗口起点（含） |
| `window_end` | `"23:59"` | 窗口终点（含，按 `23:59:59` 收尾） |
| `times` | `["21:35"]` | 自定义签到时刻，`HH:MM` 或 `HH:MM:SS`，可写多档 |
| `time_jitter_minutes` | `0` | 时间抖动 ±N 分钟，`0` 关闭 |
| `max_wait_minutes` | `12` | 早到时最多等多久，超了就放弃本次、等下一轮 |
| `tolerance_minutes` | `5` | 迟到容忍：`now - target` 在此之内算准点 |

```yaml
checkin:
  schedule:
    window_start: "21:00"
    window_end: "23:59"
    times: ["21:35", "21:50", "22:10"]
    time_jitter_minutes: 5      # 每档 ±5 分钟
    max_wait_minutes: 12
    tolerance_minutes: 5
```

> 两处默认值要分清：**代码**在完全没有 `schedule` 段时用 `["21:35"]`；**配置模板** `config.example.yaml` 里预写的是 `["21:35", "21:50"]` 两档。

**窗口 ≠ 一定签得上**：`window_*` 只是「允许脚本尝试」的范围，特意放宽到 21:00 是为了让提前档被 cron 延迟拖进窗口时还有机会执行。真正被接受的时间由 `init` 返回的 `FStartTime`–`FEndTime` 决定（随学期调整，以接口实返为准），窗口外或时段外提交会被服务端拒绝。

### 决策顺序

`server/internal/schedule`（Go）与 `src/schedule.py`（Python）是同一套规则，自上而下取第一条命中：

| # | 情况 | 动作 |
|---|---|---|
| 1 | 当前不在窗口内 | `skip`，静默退出（不登录、不推送） |
| 2 | 某档**刚过点**、仍在 `tolerance_minutes` 内 | 立刻签（`on_target`） |
| 3 | 下一档还需等待，且等待 ≤ `max_wait_minutes` | 睡到目标时刻再签 |
| 4 | 下一档等不起 | `skip`，交给后续 cron / tick |
| 5 | 所有档都已过、超出容忍 | 补签（`late_fallback`，宁可迟到不缺席） |
| 6 | 配的档全在窗口外 | 窗口内兜底签一次 |

- 第 2 条排在「等下一档」之前是刻意的：`times: ["21:35","21:50"]`、21:37 到达时，21:35 才过 2 分钟，不能因为「21:50 还差 13 分钟」就把这次机会判成跳过。
- 等待用的目标时刻**一次算好、全程复用**：不会睡醒后再抽一次抖动，把目标漂到 20 分钟后白等。
- 任意一档签成功即为当天完成，后续运行被 `FState` 判为已签到，直接静默退出。
- 窗口**不支持跨日**：`window_end <= window_start` 时打印提示并回退默认窗口。

### 时间抖动的三重保证

`time_jitter_minutes: 5` = 每档目标在 ±5 分钟内均匀随机偏移。抖动本身无约束，但有三道闸：

1. 抽到的时刻越界 → **重抽**，最多 8 次；
2. 8 次都越界 → **贴边**：低于起点的落在 `window_start` 之后 0–60 秒，高于终点的落在 `window_end` 之前 0–60 秒；
3. 真正提交前再用 `in_window()` **二次校验**，不在窗口内就放弃。

所以抖动写多大都不会在 21:00–23:59 之外打卡。

### 与 cron 的关系（Actions 侧）

脚本只能「被叫醒之后决定做什么」，不能自己创造运行机会。**cron 必须覆盖 `times` 所在的时段**，否则那个时刻根本不会被叫醒。

仓库自带 workflow 有两组档：

```yaml
- cron: "*/10 13-15 * * *"   # 北京 21:00–23:59，每 10 分钟扫一次（主力）
# 另有十档遗留 cron 兜底老配置：北京 20:05 / 20:35 / 21:05 / 21:35 / 21:50
#                                 22:05 / 23:00 / 23:05 / 23:30 / 23:55
```

- **只改 `times`（仍在 21:00–23:59 内）**：不用动 cron，10 分钟粒度够用，秒级时刻由脚本自己等到点。
- **把窗口挪出这段**（例如 `window_start: "20:00"` + `times: ["20:30"]`）：自己加 cron。北京时间 = UTC + 8，北京 20:00–23:59 = UTC 12:00–15:59 → `cron: "*/10 12-15 * * *"`；只加一个点也行，`cron: "30 12 * * *"` 就是北京 20:30。
- GitHub 定时**实测常延迟 1–4 小时**（晚间高峰尤其明显），那十档遗留 cron 就是为了对冲：被拖进窗口的运行照常签，拖到窗口外的静默退出，不会刷出一堆误报。
- Go 面板不依赖 cron：服务在窗口内每 30 秒复查一次，启动时还会补一次「今天在窗口内 + 未签到 + 已过最后一档」的 `late_fallback`。

## 位置与上报文案

### 坐标

```yaml
checkin:
  longitude: "119.20463"
  latitude: "26.04920"
  actual_location: "旗山校区 福建省福州市闽侯县乌龙江北大道2号靠近福州大学旗山校区生活区X号楼"
```

- 坐标系是**高德 GCJ-02**，与学校判定用的是同一套。面板「获取当前位置」会把浏览器给的 WGS-84 换算过来（福州实测偏移约 590 米，不换算会「定位不在签到范围」）。
- 浏览器只在 **HTTPS 或 `127.0.0.1`/`localhost`** 下允许定位；用局域网 IP 走 http 打开时按钮会直接说清楚。

### 位置池：按权重随机

```yaml
checkin:
  locations:
    - longitude: "119.20463"
      latitude: "26.04920"
      actual_location: "旗山校区 福建省福州市闽侯县乌龙江北大道2号靠近福州大学旗山校区生活区X号楼"
      weight: 3                       # 75% 概率
    - longitude: "119.20510"
      latitude: "26.04880"
      actual_location: "旗山校区 福建省福州市闽侯县乌龙江北大道2号靠近福州大学旗山校区教学楼"
      weight: 1                       # 25% 概率
```

- `locations` 非空时**完全接管**：`longitude` / `latitude` / `actual_location` 三项都不再生效。
- `weight` 可省略，默认 `1`；写 `0` 或负数按 `1` 处理（不是「永不选中」）。
- 坏项在加载时就被剔除并指出是第几项；整池无效时启动即退出，并打印可直接照抄的写法示例。
- 某一项没写 `actual_location` 时沿用外层文案，不会把上报名变成空串。

### 抖动半径

| 写法 | 生效半径 |
|---|---|
| 只填 `location_jitter_m: 300` | 300 米 |
| 只填 `jitter_radius_m: 50` | 50 米（旧键，老配置不用改） |
| 两个都填、新键非空 | 只认 `location_jitter_m`，不叠加 |
| 填 `0` | 关闭抖动，按配置坐标原样上报 |
| 负数 / 写错 / 留空 | 视为笔误，回落默认 `200` 米 |

采样在**圆面积内均匀**取点（`r = R·√U`），不是只取圆周。同一次运行里 `check`（定位预检）与 `clockIn`（提交）用**同一个**随机点，与真实设备「一次定位、两处使用」一致；抖动点若落在签到范围外会自动重取（最多 3 次），仍不行就回退基准坐标——**不会因为抖动而漏签**。

> 抖动的意义是让正常打卡的流量特征更接近真实手机（真机定位每次都有几十米漂移），**不改变「人是否真的在校」这一事实**。

### 上报地点文字：校区 + 详细地址 + 靠近 POI

学校侧上报的地点是这个形状：

```
旗山校区 福建省福州市闽侯县学府南路辅路28号靠近福州大学旗山校区生活区X号楼
└校区标记┘ └── 省 / 市 / 县 + 道路门牌 ──┘└── 靠近 + POI 全称 ──┘
```

完整规则、反例与三端实现见 [docs/address-format.md](docs/address-format.md)，要点三条：

- **「靠近」是格式的一部分**：前段是门牌级地址，后段是楼栋/POI，各管一段信息。
- **只有同一处说了两遍才合并**：高德逆地理编码已经落到楼栋时，再拼同一栋楼就是重复，这时并成一句。
- **空白折叠、不改你自己的写法**：多行 YAML、全角空格、零宽字符收成一个半角空格；只有以省市区开头的文案才会补上「旗山校区」标记。

面板的地图选点弹窗里，这栏文字有三种来源：填了高德 Web 服务 key 就按落点自动生成（个人免费额度）；粘高德分享链接时取 `name=`；或者手写一次按点位（±11 米）记在浏览器 `localStorage` 里，下次自动带出。key 与记忆都只在本机浏览器，服务端拿不到。

## 假期跳过

```yaml
vacation:
  notify: false               # 命中假期时是否推送（默认静默，避免假期刷屏）
  skip_ranges:
    - name: 寒假
      start: "2027-01-23"
      end: "2027-02-21"
    - name: 暑假
      start: "2027-07-03"
      end: "2027-08-29"
    - name: 比赛离校            # 学期中间任意区间，自己加
      start: "2026-11-02"
      end: "2026-11-08"
```

模板已按《福州大学 2026—2027 学年校历》（福大教〔2026〕10号）预填寒暑假。区间内**不签、不推、不留运行记录**（`notify: true` 才推一条提醒）；日期一过自动恢复。用 `CONFIG_YAML` secret 的话改 secret 内容即可，不用提交代码。

## 结果推送

填一种就够，其余留空（`type: none` 或整段留空 = 完全静默）：

```yaml
notify:
  type: "serverchan"          # serverchan | pushplus | bark | wecom | none
  serverchan_key: ""          # Server酱 Turbo SendKey
  pushplus_token: ""          # PushPlus token
  bark_url: ""                # Bark 完整推送 URL
  wecom_webhook: ""           # 企业微信群机器人 webhook
```

推送时机是**有信息量才推**：签到成功 ✅、签到失败 ❌（带失败原因与执行时刻）、定位不在范围 ❌、今日无考勤计划、服务器暂未返回计划。已签到 / 窗口外 / 等待超时 / 假期这些一律静默，不会一晚连推好几条。同一天同一状态只推一次；失败推送有 30 分钟去重窗口。

## 面板 API

全部挂在 `/api/v1` 下。**非 GET 请求必须带 `X-Requested-With: fzu-web`**（CSRF 双提交），登录态是 `fzu_sess` Cookie。

| 方法 | 路径 | 鉴权 | 作用 |
|---|---|:--:|---|
| `GET` | `/health` | 公开 | 健康检查（含版本） |
| `GET` | `/auth/session` | 公开 | 是否已初始化 / 是否已登录 |
| `POST` | `/auth/setup` | 公开 | 首次设置管理密码（已初始化则 409） |
| `POST` | `/auth/login` | 公开 | 登录，成功下发 `fzu_sess` |
| `POST` | `/auth/logout` | 登录 | 退出，并换会话代次（旧 Cookie 立即作废） |
| `POST` | `/auth/password` | 登录 | 改管理密码（要当前密码），成功后所有会话作废 |
| `GET` | `/config` | 登录 | 配置视图，凭据字段是 `••••` |
| `PUT` | `/config` | 登录 | 整份保存（`••••` 原样回传 = 保持原值，空串 = 清空） |
| `POST` | `/config/import-yaml` | 登录 | 粘贴旧 `config.yaml` 导入（预览 + 应用） |
| `GET` | `/config/export` | 登录 | 导出 YAML（脱敏） |
| `POST` | `/config/reveal` | 登录 | 取一项真实凭据，要再输一次管理密码，一次一项 |
| `GET` | `/status` | 登录 | 今日状态、窗口、各档、下一档、调度器状态、最近一次运行 |
| `POST` | `/control` | 登录 | 自动签到开关，只认 `{"paused":true|false}` |
| `GET` | `/runs` | 登录 | 运行历史分页 |
| `POST` | `/runs` | 登录 | 手动运行：`{"mode":"force"}` 立即签，`{"mode":"normal","dry_run":true}` 只做定位预检；受理即回 202 + `run_id`，执行在后台 |
| `GET` | `/runs/{id}` | 登录 | 单次运行详情 |
| `POST` | `/notify/test` | 登录 | 发一条测试推送 |
| `POST` | `/login/test` | 登录 | 只验证凭据，不写配置 |
| `POST` | `/login/refresh` | 登录 | 用账密/Cookie 换新 token 并写回 |
| `POST` | `/login/sms/{start,resend,verify,cancel}` | 登录 | 面板内走完短信二次验证 |
| `GET` | `/logs` | 登录 | 内存日志环快照，`?limit=` 默认 200、上限 2000（环容量 1000，新的在前） |
| `GET` | `/logs/stream` | 登录 | SSE 实时日志 |

错误统一是 `{"error":{"code":"…","message":"…","details":[]}}`；常见码：`unauthorized`、`not_initialized`、`csrf_failed`、`weak_password`、`too_many_attempts`、`config_invalid`、`run_in_progress`、`notify_not_configured`。未知 API 路径也回 JSON 404，不会给前端一坨纯文本。

```bash
# 登录（拿到 Cookie 后其余接口都带上）
curl -s -c jar -H 'X-Requested-With: fzu-web' -H 'Content-Type: application/json' \
     -d '{"password":"你的管理密码"}' http://127.0.0.1:14514/api/v1/auth/login

# 今日状态
curl -s -b jar http://127.0.0.1:14514/api/v1/status

# 手动补签（跳过等待与窗口拦截）
curl -s -b jar -H 'X-Requested-With: fzu-web' -H 'Content-Type: application/json' \
     -d '{"mode":"force"}' http://127.0.0.1:14514/api/v1/runs

# 定位预检（只 init + check，不打卡、不推送）
curl -s -b jar -H 'X-Requested-With: fzu-web' -H 'Content-Type: application/json' \
     -d '{"mode":"normal","dry_run":true}' http://127.0.0.1:14514/api/v1/runs

# 实时日志
curl -N -b jar http://127.0.0.1:14514/api/v1/logs/stream
```

## 一次签到做了什么

```
调度触发（面板 tick / 启动补签 / Actions cron / 手动）
  └─ 读配置 → 解析 schedule（窗口、各档、抖动、等待、容忍）
       ├─ 不在窗口 / 等不起 / 已暂停 → 静默退出（不登录、不推送）
       └─ 到了目标时刻（早到就等到点）
            ├─ 假期区间命中 → 跳过（默认不推送、不留记录）
            ├─ 无可用凭据 → skipped（提醒去配，不谎报"登录失败"）
            ├─ 无 token → 学号密码走 SSO（或 Cookie 续票）换 token 并回写配置
            ├─ init.action 查今日计划与状态
            │    ├─ FState = 已签到 → 静默结束
            │    ├─ 无 schoolData → "今日无考勤计划"
            │    ├─ token 被上游拒（回 HTML）→ 重取一次 token 后重试
            │    └─ 跨午夜 500 → 记一次，稍后重试（有推送去重）
            ├─ resolve_location：单点 或 位置池按权重抽一个基准点
            ├─ jitter_location：半径内按面积均匀取点
            ├─ check.action：验证这个点是否落在 schoolData 的多边形里
            │    └─ 越界 → 重取（最多 3 次）→ 仍越界则回退基准点
            ├─ currentTimestamp.action：拿服务器时间 → isLate
            ├─ clockIn.action：提交 campus / lon / lat / actualLocation / isLate
            └─ notify：成功 / 失败 / 不在范围，按配置推送到微信（同日同状态去重）
```

运行结论落 `data/runs.jsonl`（面板「历史」页可看），状态取值范围 `success` / `already_checked` / `no_plan` / `out_of_range` / `vacation` / `skipped` / `error`，模式取值 `scheduled` / `force` / `manual` / `probe`。

## 数据与安全

### 数据目录

只由 `-data` / `FZU_DATA_DIR` 决定，目录里是这台部署的全部真源：

| 文件 | 内容 |
|---|---|
| `config.json` | 配置；凭据字段是 `enc:v1:` 前缀的 AES-256-GCM 密文 |
| `config.json.bak` | 上一份配置（同样密文；从明文老配置升级的第一次保存**不写** `.bak`） |
| `state.json` | 最近成功日期、上一档 key、推送去重表、暂停开关、今日计划缓存 |
| `runs.jsonl` | 运行历史，一行一次；超过 2000 行轮转到 `runs.jsonl.1` |
| `auth.json` | 管理密码哈希（`pbkdf2$sha256$600000$…`）、初始化时间、会话代次 |
| `secret.key` | 配置加密与会话签名的密钥材料 |

### 面板侧

- **管理密码**：PBKDF2-HMAC-SHA256 **60 万轮**；登录成功时会把 12 万轮的老哈希顺手续到当前参数（不踢下线）。
- **会话**：HMAC 签名的 `fzu_sess` Cookie，7 天；载荷里带**会话代次** `sv`，改密码或点退出就换代次——所有旧 Cookie 当场作废（包括被复制走的那张）。
- **限速**：登录 / 改密失败同 IP 15 分钟 5 次触发 429；「显示凭据」的失败计数独立，手滑输错密码不会顺手锁登录。限速身份默认取 TCP 来源地址，只有 `-trust-proxy` 才采信 `X-Forwarded-For`。
- **CSRF / CSP / 缓存**：非 GET 必须带 `X-Requested-With: fzu-web`；API 响应 `Cache-Control: no-store`；页面 CSP 只放行同源脚本样式，外加地图弹窗需要的 `*.autonavi.com` / `tile.openstreetmap.org` 瓦片和 `restapi.amap.com` 逆地理。
- **脱敏**：`GET /config`、配置页、日志、SSE、`runs.jsonl`、推送正文里的凭据一律打码；`logf` 在写日志前还有一道独立脱敏，避免某个调用点忘掉。
- **目录权限**：Linux/macOS 目录 `0700`、文件 `0600`；Windows 启动时用 `icacls` 去掉继承、只授权本账号 + SYSTEM + Administrators（`FZU_SKIP_ACL=1` 可跳过），并做一次建/写/读/删自检。
- **顺序**：先用 `127.0.0.1` 设好管理密码，再对外监听。未初始化 + 非回环监听 = 谁先连上谁就是管理员，启动横幅会明确警告。

### 对外暴露

只在可信反向代理后面开 https，服务本身监听 `127.0.0.1`。代理要点：`proxy_buffering off;`（SSE）、`proxy_read_timeout 300s;`、传递 `X-Forwarded-Proto`，并给服务加 `-trust-proxy`。

```nginx
location / {
    proxy_pass http://127.0.0.1:8787;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;
    proxy_read_timeout 300s;
}
```

Linux 常驻用 systemd（`ExecStart=/usr/local/bin/fzucheckin -listen 127.0.0.1:8787 -data /var/lib/fzucheckin`，`Restart=on-failure`、`NoNewPrivileges=yes`、`ProtectSystem=strict`、`ReadWritePaths=/var/lib/fzucheckin`）；Windows 用计划任务开机启动。

### Actions / Python 侧

- `config.yaml` 是明文，权限靠 `0600` 与「私有仓库 + Secret」；**不要**把它放进公开仓库或共享目录。
- `CONFIG_YAML` secret 的内容每次运行临时落盘，回写的 token 不会持久化——每次运行都会重新登录一次，这是预期行为。
- SSO 自动登录遇到验证码会明确失败，此时改用 token（[docs/get-token.md](docs/get-token.md)）。

## 项目结构

```
fzu-auto-checkin/
├── main.py                     # Python 入口：调度决策 → 假期 → 登录 → 查询 → 打卡 → 推送
├── src/
│   ├── schedule.py             # 窗口 / 多档 / 时间抖动 / 二次校验（纯函数）
│   ├── config.py               # 配置加载与规范化（YAML 60 进制、坏坐标、坏类型都降级并提示）
│   ├── checkin.py              # 接口客户端 + 位置解析 + 位置抖动 + 打卡链路
│   ├── login.py                # SSO 登录（AES-128-ECB 加密挑战值）换免登 token
│   ├── notify.py               # Server酱 / PushPlus / Bark / 企业微信
│   └── vacation.py             # 假期区间匹配
├── server/
│   ├── cmd/fzucheckin/main.go   # 参数解析 → 打开数据目录 → 装配服务 → 优雅退出
│   ├── internal/config/         # 配置唯一出入口：默认模板、规范化、校验、脱敏、YAML 导入导出
│   ├── internal/schedule/       # 调度内核（与 Python 逐条等价）
│   ├── internal/fzu/            # 学校协议：4 个接口、SSO、Cookie 续票、短信验证、推送、假期
│   ├── internal/secure/         # AES 参数加密 / AES-ECB / PBKDF2 / 配置封装 / 会话签名
│   ├── internal/store/          # data/ 的唯一入口：原子写、坏文件降级、权限收紧
│   ├── internal/server/         # 路由与中间件、鉴权、调度循环、一次签到的编排、SSE
│   ├── internal/model/          # 共享类型（配置、状态、运行记录、上游结构）
│   └── web/static/              # 原生前端：index.html / app.js / style.css / map-picker.js
├── web/
│   ├── configurator.html       # 离线配置生成器（可一键建私有仓库并写入配置）
│   ├── promo.html              # 宣传卡
│   └── qr.png                  # 页面素材
├── docs/
│   ├── protocol.md             # 接口协议逆向笔记（4 个接口 + 加密 + FPosition 形状坑）
│   ├── address-format.md       # 上报地点文案的规范与三端实现
│   ├── schedule-design.md      # 调度层契约与决策表
│   ├── go-service-design.md    # Go 服务设计（路由、错误码、并发与关闭顺序）
│   ├── server-readme.md        # Go 面板使用说明（更细的操作手册）
│   ├── get-token.md            # 用控制台脚本把 token 交给面板
│   ├── capture.md              # 自己抓包验证报文的方法
│   ├── ui-redesign.md          # 界面改版记录与令牌
│   └── assets/                 # logo.png / promo.png（品牌图，唯一允许入库的图片）
├── scripts/                    # repo-guard.sh、密钥扫描、Auto.js 脚本
├── .github/workflows/          # checkin.yml（签到）、repo-guard.yml、secret-scan.yml
├── .githooks/pre-commit        # 路径闸门 + 密钥扫描
├── config.example.yaml         # 配置模板（每项都有注释）
├── start.ps1 / start.cmd       # Windows 一键启动（按需构建 → 前台运行 → 打开浏览器）
└── betterleaks.toml            # 密钥扫描规则
```

## 构建与测试

```bash
# Go：静态检查 + 全部离线测试（假学校上游、临时数据目录）
cd server
go vet ./... && gofmt -l .
go test -count=1 ./...

# Python：单元测试（tests/ 不入库，见下）
python -m pytest tests -q

# 本机一致性工具（同样不入库）：重放 Python 生成的调度语料、生成 AES 向量
python scripts/gen_parity.py
```

- 本机实测：`go test -count=1 ./...` 全绿（7 个带测试的包，443 个 `Test*` 函数）；`pytest` **145 passed**（96 个测试函数，参数化后 145 例）。两边都**不联网、不读真实配置**，时间与随机源全部注入。
- **跨语言一致性**：`internal/schedule/parity_test.go` 重放 Python 版生成的 323 例决策语料（六条分支、抖动钳制、边界秒）。
- **加密字节级对齐**：`internal/secure/secure_test.go` 用 Python 生成的密文/明文向量断言 AES-CBC 与 AES-ECB 输出逐字节相同（含中文 `\uXXXX` 转义）。
- **端到端冒烟**：本机脚本会构建 → 启动 → 初始化 → 配置校验 → 脱敏回显 + 二次校验取值 → 落盘密文校验 → SSE → 优雅退出。
- 约定：调度逻辑只写在 `src/schedule.py` / `internal/schedule`，不许在入口里比较时刻；窗口判定只有 `in_window()` 一条路径。

## 仓库里不放什么

**构建产物、冒烟脚本与冒烟产物、测试与测试数据、截图、运行时数据、真实配置**一律不入库。规则只有一份（[`scripts/repo-guard.sh`](scripts/repo-guard.sh)），三道闸门把着：

- `.gitignore`：挡还没被跟踪的文件；
- 仓库内 pre-commit（`.githooks/pre-commit`）：路径闸门 + 密钥扫描，任一不过就取消提交；
- CI（`.github/workflows/repo-guard.yml` + `secret-scan.yml`）：扫整棵已跟踪树与**全历史**，`git add -f` 或 `--no-verify` 绕过的提交照样红。

图片只允许两类：页面素材 `web/qr.png` 与品牌图 `docs/assets/{logo,promo}.png`（生成图，不含任何本机信息）；其余 `*.png/jpg/webp/gif` 一律视为可能带本机路径、坐标或账号的截图。

新克隆先启用钩子（`core.hooksPath` 可能被全局配置指着别处，仓库内钩子默认不生效）：

```bash
git config core.hooksPath .githooks
sh ./scripts/repo-guard.sh --tree     # 手动全树体检，CI 跑的就是这条
```

密钥扫描用 [betterleaks](https://github.com/betterleaks/betterleaks)（gitleaks 原作者的新项目），规则与豁免在根目录 `betterleaks.toml`，与本地钩子共用同一份配置。

## 已知限制

- **窗口不支持跨日**（如 23:00–01:00），写了会回退默认窗口并提示。
- Actions 侧：自定义 `times` 必须落在 cron 扫描的时段里，否则不会被叫醒；GitHub 定时本身**不保证准点**，延迟 1–4 小时是常态，靠「窗口扫描 cron + 十档遗留档 + 多档时刻 + 迟到补签」把漏签概率压到最低，但无法承诺 100% 准点。
- 签到完全依赖学校 H5 的接口与字段。若前端改版（路径、字段名、加密密钥变化），需要重新逆向；报错会明确推送，不会静默失败。
- 校外 / 机房 IP 上统一身份认证会强制短信二次验证；服务端只能替你提交验证码，替不了收短信（面板内可完成，Python 侧需手填 token）。
- 面板是**单用户、单实例**：一个进程一个写者，不做多用户与多副本；同一份 `data/` 不要给两个进程同时用。
- 服务器要求图形验证码时 SSO 自动登录会失败，此时手动获取 token 填进 `user.token`。
- `web/configurator.html` 只覆盖账号 / 位置 / 推送 / 假期四块，**不含 `schedule` 段**；要自定义时刻，部署完再手写 6 行。

## 常见问题

<details>
<summary><b>token 是什么？会过期吗？</b></summary>

登录态凭证。**推荐填学号密码**：服务会用 SSO 自动换 token 并回写配置，全程无感。有效期官方没公开，失效就重新登录一次（配上账密或 Cookie 就自动做了）。在 GitHub Actions 里 `config.yaml` 每次从 Secret 临时生成，回写的 token 不会持久化——每次运行都会重新登录一次，这是预期行为。
</details>

<details>
<summary><b>迟到线是几点？</b></summary>

以服务器返回的 `FLateTime` 为准（现行约 **22:30**）：21:30–22:30 正常，此后签到标记 `isLate=1`。服务用服务器时间戳算迟到，不看 runner 或宿主机的本地时钟。
</details>

<details>
<summary><b>为什么本地跑的时候直接退出了？</b></summary>

大概率是当前时间不在窗口内（会打印「北京时间 xx:xx 不在签到窗口 21:00-23:59 内」）。这是设计行为：窗口外的运行一律静默跳过，避免延迟触发的补跑刷出误报。想立刻试一次就加 `CHECKIN_FORCE=1`，或点面板的「立即签到」。
</details>

<details>
<summary><b>面板打不开 / 打开只看到「请通过服务访问」？</b></summary>

- 「请通过服务访问」= 直接双击了 `index.html`（`file://`），必须走服务 URL。
- 登录后立刻回登录页：多半是 Cookie 被浏览器拦了——用 `http://127.0.0.1:14514` 与 `http://localhost:14514` 时别混用，或者反代没透传 `X-Forwarded-Proto: https`。
- 换端口后用 `-listen` / `-Port` 指定；`start.ps1 -Status` 可以确认到底在不在跑。
</details>

<details>
<summary><b>忘了管理密码怎么办？</b></summary>

停下服务，把 `data/auth.json` 移走再启动，会回到「设置管理密码」这一步；配置、历史与凭据都还在（它们分别存在 `config.json` 与 `secret.key` 里），重新配个密码即可。
</details>

<details>
<summary><b>报「服务器暂未返回今日计划」？</b></summary>

跨午夜时段（约 0:00–1:00）服务器还没发布当日计划，`init` 会返回 500，属正常现象。脚本会在首跑推送一次提醒，之后的重试静默，不会轰炸你。
</details>

<details>
<summary><b>一晚会不会重复打卡？</b></summary>

不会。任意一档签成功后，后续每次运行都会被 `FState` 判成「已签到」直接静默退出；面板还有单飞开关（同一时刻最多一次运行）与 `last_slot_key` 幂等闸；Actions 侧有 `concurrency` 串行化。
</details>

<details>
<summary><b>iPhone 能用吗？</b></summary>

能，而且最省事：跑在云端（或家里常开的机器上），iPhone 只负责收微信推送，什么都不用装。想自己抓包看报文才需要 Android 或电脑代理。
</details>

<details>
<summary><b>人不在学校会怎样？</b></summary>

脚本不做真实定位、也不伪造 GPS——这是特性也是底线。**请在本人确实在校时使用**。长期离校（实习、交换、请假）请走辅导员报备，别用工具掩盖状态。
</details>

<details>
<summary><b>大概要花多少 Actions 额度？</b></summary>

窗口内每 10 分钟扫一次、单次约几十秒，一晚上约 20 次运行；真正干活的只有到目标时刻那一次，其余在 `init` 之后立刻退出。公开库免费，私有库也在免费额度内（每月 2000 分钟）。
</details>

## 贡献与反馈

- 用着有问题、发现 bug、想要新功能：直接提 Issue。
- 适配其他学校的同类系统：欢迎 PR。**提交前请脱敏**：学号、密码、token、真实住址一律打码。
- 提交前跑一遍 `sh ./scripts/repo-guard.sh --tree` 与 `go test -count=1 ./...`。
- 本项目不接受、不鼓励任何形式的代挂与有偿使用。

## 致谢

- 福州大学「智汇福大」晚点名 H5 的接口形态是静态分析与实测的产物，笔记全部在 [docs/protocol.md](docs/protocol.md)。
- 地图选点用的是高德公开瓦片与 OpenStreetMap 兜底；本宣传图与 logo 由腾讯混元生图生成。

## License

[MIT](LICENSE) © 2026 fzu-auto-checkin contributors
