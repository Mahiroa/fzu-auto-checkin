<div align="center">

# 🎓 fzu-auto-checkin

**福州大学「智汇福大」晚点名 · 云端自动签到**

配置一次，剩下的交给 GitHub Actions：到点自动打卡，结果推到你微信。

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Python](https://img.shields.io/badge/python-3.11%2B-blue.svg)](requirements.txt)
[![Tests](https://img.shields.io/badge/tests-129%20passed-brightgreen.svg)](tests)
[![Cron](https://img.shields.io/badge/cron-21%3A00--23%3A59%20%2F%2010min-green.svg)](.github/workflows/checkin.yml)

<img src="assets/fzu-checkin-poster.png" width="360" alt="晚点名自动签到海报">

</div>

> ⚠️ **使用须知**
>
> 晚点名是学校的安全确认制度。本项目只用于**本人确实在校时防止漏签**：不伪造定位、不隐瞒真实在/离校状态、不接受付费代挂。违规使用的后果由使用者自行承担。

> 📌 海报和宣传卡里的二维码指向旧的在线配置页，已经下线。**请用仓库内的 [`web/configurator.html`](web/configurator.html)**（下载后双击即可离线使用）。

---

## ✨ 功能

| | 能力 | 说明 |
|---|---|---|
| ⏰ | **自定义签到时刻** | `schedule.times` 想写几档写几档，如 `["21:35", "22:10"]`；被 cron 提前叫醒会自动等到最近一档 |
| 🎲 | **时间抖动** | `schedule.time_jitter_minutes` 给每档 ±N 分钟随机偏移，抖动越界先重抽、再贴边、执行前还会二次校验，**永远落不到 21:00–23:59 之外** |
| 📍 | **自定义位置** | 单点坐标继续可用；`locations` 位置池按权重随机（宿舍 3 : 教学楼 1），上报地址文案跟着点走 |
| 🌐 | **位置抖动** | 以选中点为圆心、默认 200 米半径的圆内均匀取点，不再每晚报同一个经纬度 |
| 🔑 | **免维护认证** | 学号密码走 SSO 自动换 token（会自动回写配置）；也可直接填免登 token |
| 🎯 | **定位预检** | 打卡前先调 `check` 接口验证坐标是否在签到范围内，不在就明确报错，绝不乱点 |
| 📲 | **结果推送** | Server酱 / PushPlus / Bark / 企业微信群机器人，四选一；也可完全静默 |
| 🌴 | **假期跳过** | 按校历预填寒暑假，也能自己加任意免签区间（实训、比赛离校、调休） |
| 🕘 | **手动补签** | 错过定时就手动跑一次：Actions 里勾 `force`，或本地 `CHECKIN_FORCE=1` |
| 🧪 | **离线可测** | 129 个单元测试不联网、不读真实配置，时间与随机源全部可注入 |
| 🤖 | **备选方案** | 附 v0.1 Auto.js 无障碍脚本，模拟点击、不碰接口，供参考 |

技术路线是**接口级**而非模拟点击：静态分析晚点名 H5 前端，逆向出 4 个接口与 AES 参数加密，再用 HTTP 完整复现。不用 Frida、不用抓包也能在云端跑。

## 🚀 快速开始（3 步）

### 1️⃣ 拿到代码

```bash
git clone https://github.com/Mahiroa/fzu-auto-checkin.git
cd fzu-auto-checkin
pip install -r requirements.txt
```

### 2️⃣ 填配置

```bash
cp config.example.yaml config.yaml
```

`config.yaml` 已被 `.gitignore` 忽略，不会被提交。**最小可用只需要 4 项**：

```yaml
user:
  username: "你的学号"          # 填学号密码即可，脚本自己换 token
  password: "登录智汇福大 App 的密码"

checkin:
  longitude: "119.20463"       # 高德坐标拾取器量一下宿舍楼
  latitude: "26.04920"
```

| 配置项 | 填什么 | 去哪拿 |
|---|---|---|
| `user.username` / `password` | 登录智汇福大 App 的学号密码（**推荐**） | 你平时登 App 用的那套 |
| `user.token` | 免登 token（与账密二选一，配了 token 就不走 SSO） | 见 [docs/protocol.md](docs/protocol.md) §4 |
| `checkin.longitude` / `latitude` | 校区内常驻位置的坐标（高德 GCJ-02） | [高德坐标拾取器](https://lbs.amap.com/tools/picker) → 点你的宿舍楼 |
| `checkin.actual_location` | 上报的地址文案 | 如「福州大学旗山校区生活区X号楼」 |
| `notify.*` | 推送渠道，填一种 | [Server酱](https://sct.ftqq.com) / [PushPlus](https://www.pushplus.plus) 注册即得 key |

> 🖱️ **不想手填？** 用 [`web/configurator.html`](web/configurator.html)：纯前端单页，跟着「账号 → 位置 → 推送」三步走，填完直接生成 `config.yaml`。有 GitHub Token 的话还能一键建私有仓库、从本仓库拉取代码并把配置一起写进去（[`web/promo.html`](web/promo.html) 是配套宣传卡）。
>
> 生成器目前只覆盖账号 / 位置 / 推送 / 假期四块，**不含 `schedule` 段**——要自定义时刻，部署完再照下面的「自定义签到时间」手动加 6 行。

### 3️⃣ 跑起来

**先本地验证一次**（确认坐标和账号没问题）：

```bash
python main.py
```

看到「已签到 / 无需操作」或「签到成功 ✅」就说明配置对了。当前时间不在签到窗口内会打印一句原因直接退出，这是正常的（不是报错）。

**再挂到云端自动跑**（推荐，手机零安装）：

1. Fork 本仓库，或新建一个 **私有仓库** 把代码推上去
2. `Settings → Secrets and variables → Actions → New repository secret`，名称 `CONFIG_YAML`，值 = 你本地 `config.yaml` 的**全文**
3. 完事。Actions 每 10 分钟扫一次北京时间 21:00–23:59，到点自动签到

> 💡 **为什么建议私有仓库**：凭据只经 Secret 注入，不进代码库；私有库的 Actions 免费额度对「每晚跑几次」完全够用。
>
> 💡 **也可以直接提交 `config.yaml`**：workflow 的逻辑是「有 `CONFIG_YAML` secret 就用它，否则用仓库里的 `config.yaml`」。走这条路记得 `git add -f config.yaml`（`.gitignore` 默认忽略它），并且**必须是私有仓库**。

## ⏰ 自定义签到时间

`checkin.schedule` 整段都可以不写，缺省等价于「每天 21:35 签一次，窗口 21:00–23:59」，跟老版本行为一致。所有时刻一律按**北京时间（UTC+8）**解释，与本机时区、runner 的 UTC 都无关。

下表省略了 `checkin.` 前缀（它们都在 `checkin.schedule` 下面）：

| 键 | 默认值 | 含义 |
|---|---|---|
| `schedule.window_start` | `"21:00"` | 允许脚本动手的窗口起点（含） |
| `schedule.window_end` | `"23:59"` | 窗口终点（含，按 `23:59:59` 收尾） |
| `schedule.times` | `["21:35"]` | 自定义签到时刻，`HH:MM` 或 `HH:MM:SS`，可写多档 |
| `schedule.time_jitter_minutes` | `0` | 时间抖动 ±N 分钟，`0` 关闭 |
| `schedule.max_wait_minutes` | `12` | 早到时最多等多久，超了就放弃本次、等下一个 cron |
| `schedule.tolerance_minutes` | `5` | 迟到容忍：`now - target` 在此之内算准点 |

> 两处默认值的区别：**代码**在完全没有 `schedule` 段时用 `["21:35"]`；**配置模板** `config.example.yaml` 里预写的是 `["21:35", "21:50"]` 两档。跟模板走就是两档兜底，删掉这个键就是单档。

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

**窗口 ≠ 一定能签成功**：`window_*` 只是「允许脚本尝试」的范围，特意放宽到 21:00 是为了让提前档在被 GitHub 定时延迟拖进窗口时还有机会执行。服务器真正接受签到的时间由 `init` 返回的 `FStartTime`–`FEndTime` 决定（现行约 21:30–23:59），窗口外或时段外提交会被服务端拒绝。

### 多档与调度顺序

- 每个时刻是一档目标时刻；写 `HH:MM:SS` 也可以，**务必带引号**（不加引号 YAML 会按 60 进制把 `21:35` 解析成 `1295`，脚本会尽量还原并警告）。
- 决策顺序（`src/schedule.py`，自上而下取第一条命中）：

  | # | 情况 | 动作 |
  |---|---|---|
  | 1 | 当前不在窗口内 | `skip`，静默退出（不登录、不推送） |
  | 2 | 某档**刚过点**、仍在 `tolerance_minutes` 内 | 立刻签（`on_target`） |
  | 3 | 下一档还需等待，且等待 ≤ `max_wait_minutes` | 睡到目标时刻再签 |
  | 4 | 下一档等不起 | `skip`，交给后续 cron |
  | 5 | 所有档都已过、超出容忍 | 补签（`late_fallback`，宁可迟到不缺席） |
  | 6 | 配的档全在窗口外 | 窗口内兜底签一次 |

- 第 2 条排在「等下一档」之前是刻意的：`times: ["21:35","21:50"]`、21:37 到达时，21:35 才过 2 分钟，不能因为「21:50 还差 13 分钟」就把这次机会判成跳过。
- 等待用的目标时刻**一次算好、全程复用**：不会睡醒后再抽一次抖动，把目标漂到 20 分钟后白等。
- 任意一档签成功即为当天完成，后续 cron 被 `FState` 判为已签到，直接静默退出，不会重复打卡。
- 窗口**不支持跨日**：`window_end <= window_start` 时会打印提示并回退默认窗口。

### 时间抖动：三重保证不出界

`time_jitter_minutes: 5` = 每档目标在 ±5 分钟内均匀随机偏移（`0` 关闭）。抖动本身是无约束的，但有三道闸：

1. 抽到的时刻越界 → **重抽**，最多 8 次（`MAX_JITTER_REDRAWS`）；
2. 8 次都越界 → **贴边**：低于起点的落在 `window_start` 之后 0–60 秒，高于终点的落在 `window_end` 之前 0–60 秒；
3. 真正提交前再用 `in_window()` **二次校验**，不在窗口内就放弃。

所以 `time_jitter_minutes` 写多大都不会在 21:00–23:59 之外打卡——实测 ±30 分钟抖动跑 200 轮 × 400 个目标，越界 0 个。

### 与 GitHub cron 的关系

脚本只能「被叫醒之后决定做什么」，不能自己创造运行机会。**cron 必须覆盖 `schedule.times` 所在的时段**，否则那个时刻根本不会被叫醒。

仓库自带的 [.github/workflows/checkin.yml](.github/workflows/checkin.yml) 有两组档：

```yaml
- cron: "*/10 13-15 * * *"   # 北京 21:00–23:59，每 10 分钟扫一次（主力）
# 另有十档遗留 cron 兜底老配置：北京 20:05 / 20:35 / 21:05 / 21:35 / 21:50
#                                 22:05 / 23:00 / 23:05 / 23:30 / 23:55
```

- **只改 `times`（仍在 21:00–23:59 内）**：不用动 cron，10 分钟粒度够用，秒级时刻由脚本自己等到点。
- **把窗口挪出这段**（例如 `window_start: "20:00"` + `times: ["20:30"]`）：自己加 cron。北京时间 = UTC + 8，北京 20:00–23:59 = UTC 12:00–15:59 → `cron: "*/10 12-15 * * *"`；只加一个点也行，`cron: "30 12 * * *"` 就是北京 20:30。
- GitHub 的定时**实测常延迟 1–4 小时**（晚间高峰尤其明显），那十档遗留 cron 就是为了对冲：被延迟拖进窗口的运行照常签，拖到窗口外的静默退出，不会刷出一堆误报。
- 多个 cron 撞在一起也不会重复签到：workflow 有 `concurrency` 串行化，脚本本身也有 `FState` 幂等。

## 📍 自定义位置与位置抖动

### 单点坐标（老配置，继续可用）

```yaml
checkin:
  longitude: "119.20463"
  latitude: "26.04920"
  actual_location: "福州大学旗山校区生活区X号楼"
```

### 位置池：按权重随机

```yaml
checkin:
  locations:
    - longitude: "119.20463"
      latitude: "26.04920"
      actual_location: "福州大学旗山校区生活区X号楼"
      weight: 3                       # 75% 概率
    - longitude: "119.20510"
      latitude: "26.04880"
      actual_location: "福州大学旗山校区教学楼"
      weight: 1                       # 25% 概率
```

- `locations` 非空时**完全接管**：`longitude` / `latitude` / `actual_location` 三项都不再生效。
- `weight` 可省略，默认 `1`；写 `0` 或负数按 `1` 处理（不是「永不选中」）。
- 每一项都必须有能当数字用的坐标；坏项在加载配置时就被剔除并指出是第几项。整池都无效时启动即退出，并打印可直接照抄的写法示例。
- 某一项没写 `actual_location` 时沿用外层文案，不会把上报名变成空串。

### 抖动半径

| 写法 | 生效半径 |
|---|---|
| 只填 `location_jitter_m: 300` | 300 米 |
| 只填 `jitter_radius_m: 50` | 50 米（旧键，老配置不用改） |
| 两个都填、新键非空 | 只认 `location_jitter_m`，不叠加 |
| 填 `0` | 关闭抖动，按配置坐标原样上报 |
| 负数 / 写错 / 留空 | 视为笔误，回落默认 `200` 米（负号手滑不该静默关掉抖动） |

```yaml
checkin:
  jitter_radius_m: 200        # 旧键
  location_jitter_m: ""       # 新键，非空即接管
```

采样在**圆面积内均匀**取点（`r = R·√U`），不是只取圆周，所以不会露出「每次贴边」的规律。同一次运行里 `check`（定位预检）和 `clockIn`（提交）用**同一个**随机点，与真实设备「一次定位、两处使用」一致；抖动点若落到签到范围外会自动重取（最多 3 次），仍不行就回退到配置坐标——**不会因为抖动而漏签**。

> 🌐 抖动的意义是让正常打卡的流量特征更接近真实手机（真机定位每次都有几十米漂移），**不改变「人是否真的在校」这一事实**。

## 🌴 假期跳过

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

模板已按《福州大学 2026—2027 学年校历》（福大教〔2026〕10号）预填寒暑假。区间内脚本**不签、不推、不留记录**，Actions 照常触发但等于空转；日期一过自动恢复。用 `CONFIG_YAML` secret 的话，改 secret 内容即可，不用提交任何代码。

## 📲 推送配置

填一种就够，其余留空：

```yaml
notify:
  type: "serverchan"          # serverchan | pushplus | bark | wecom | none
  serverchan_key: ""          # Server酱 Turbo SendKey
  pushplus_token: ""          # PushPlus token
  bark_url: ""                # Bark 完整推送 URL
  wecom_webhook: ""           # 企业微信群机器人 webhook
```

推送时机是**有信息量才推**：签到成功 ✅、签到失败 ❌（带失败原因与执行时刻）、定位不在范围 ❌、今日无考勤计划、服务器暂未返回计划。已签到 / 窗口外 / 等待超时这些情况一律静默，不会一晚连推好几条。

## 🕘 手动补签

错过定时、人确实在校要补一次时，跳过「等到目标时刻」立即执行。

**云端**：仓库 → `Actions` → 左侧 `checkin` → `Run workflow` → 勾上 `force` → 运行。

**本地**：

```bash
# bash / macOS / Linux
CHECKIN_FORCE=1 python main.py

# Windows PowerShell
$env:CHECKIN_FORCE = "1"; python main.py
```

`CHECKIN_FORCE=1` 会跳过等待与窗口外拦截，但仍会打印当前时间与窗口，窗口外会先给一行 `[警告] 手动强制补签（窗口外）` 再照常尝试——补签本来就是窗口外才用得上。其余行为不变：假期区间、已签到判定、失败/成功推送全部照旧。

## 🗺 一次签到发生了什么

```
GitHub cron 触发
  └─ 读 config（secret 优先）→ 解析 schedule
       ├─ 不在窗口 / 等不起 → 打印原因，静默退出（不登录、不推送）
       └─ 到了目标时刻（早到就 sleep 到点）
            ├─ 假期区间命中 → 跳过
            ├─ 无 token → 学号密码走 SSO 换 token（并回写）
            ├─ init.action 查今日计划与状态
            │    ├─ FState = SUCCESS/已签到 → 静默结束
            │    ├─ 无 schoolData → 「今日无考勤计划」
            │    └─ 跨午夜服务器 500 → 推送一次提醒，稍后 cron 重试
            ├─ resolve_location：单点或位置池按权重选一个基准点
            ├─ jitter_location：在半径内圆面积均匀取点
            ├─ check.action：验证这个点是否落在 schoolData 的多边形里
            │    └─ 越界 → 重取（最多 3 次）→ 仍越界则回退基准点
            ├─ currentTimestamp.action：拿服务器时间，与 FLateTime 比 → isLate
            ├─ clockIn.action：提交 campus / lon / lat / actualLocation / isLate
            └─ notify：成功 / 失败 / 不在范围，按配置推送到微信
```

## 🧩 目录结构

```
fzu-auto-checkin/
├── main.py                     # 入口：调度决策 → 假期 → 登录 → 查询 → 打卡 → 推送
├── src/
│   ├── schedule.py             # 时间调度：窗口 / 多档时刻 / 时间抖动 / 二次校验（纯函数）
│   ├── config.py               # 配置加载与规范化，友好报错（YAML 60 进制、坏坐标、坏类型）
│   ├── checkin.py              # 接口客户端 + 定位解析 + 位置抖动 + 打卡全链路
│   ├── login.py                # SSO 自动登录（AES-128-ECB 挑战值加密）换免登 token
│   ├── notify.py               # Server酱 / PushPlus / Bark / 企业微信
│   └── vacation.py             # 假期区间匹配
├── config.example.yaml         # 配置模板（每一项都有注释）
├── docs/
│   ├── protocol.md             # 接口协议逆向笔记（4 个接口 + 加密）
│   ├── capture.md              # iOS 抓包教程（想自己验证报文时看）
│   └── schedule-design.md      # 时间调度层的接口契约与决策表
├── scripts/
│   └── autojs_wandianming.js   # v0.1 Auto.js 无障碍脚本（模拟点击备选方案）
├── web/
│   ├── configurator.html       # 离线配置生成器（可一键部署到私有仓库）
│   └── promo.html              # 宣传卡
├── assets/                     # 海报与二维码（README / 宣传卡用图）
├── tests/                      # 129 个离线用例
└── .github/workflows/checkin.yml
```

## 🧪 本地开发与测试

```bash
pip install -r requirements.txt   # requests / PyYAML / pycryptodome
python -m pytest tests -q         # 129 passed
```

测试全部离线：不联网、不读你的真实 `config.yaml`、随机源与当前时间都可注入，所以任何时刻、任何机器上结果一致。

覆盖的东西：窗口边界（20:59:59 / 21:00:00 / 23:59:59 / 00:00:00）、抖动越界重抽与贴边、六种决策分支、位置池加权与坏坐标、位置抖动半径约束、`main()` 在窗口外零请求零推送、配置模板与 workflow 的键位一致性。

改代码时的两个约定：调度逻辑只写在 `src/schedule.py`，不许在 `main.py` 里比较时刻；窗口判定只有 `in_window()` 一条路径。

## 🔍 工作原理

晚点名是个普通 H5 页面，认证只靠一个免登 token（H5 URL 的 `?token=` → 请求头 `token`）。本项目通过静态分析前端 JS 逆向出完整协议：

- **4 个接口**：`init.action` 查计划 / `currentTimestamp.action` 校时 / `check.action` 定位预检 / `clockIn.action` 提交
- **参数加密**：AES-128-CBC，`KEY = IV = apexinfoapexinfo`（前端硬编码），PKCS7 + Base64，双重 URL 编码后放进 `param=`
- **登录链路**：`sso.fzu.edu.cn` 登录页下发一次性挑战值，密码用 `AES-128-ECB(croypto)` 加密后提交，成功后从 302 链里取 `token=`

完整笔记见 [docs/protocol.md](docs/protocol.md)。另附 v0.1 Auto.js 无障碍脚本（[scripts/autojs_wandianming.js](scripts/autojs_wandianming.js)）：模拟点击「业务 → 晚点名签到 → 晚点打卡」，不碰接口、不伪造 GPS，适合想在手机上跑的人。

## ❓ 常见问题

<details>
<summary><b>token 是什么？会过期吗？</b></summary>

登录态凭证。**推荐直接填学号密码**：脚本会用 SSO 自动换 token 并回写 `config.yaml`，全程无感。token 有效期官方没公开，失效就重新登录一次（配上账密就自动做了）。

注意：在 GitHub Actions 里，`config.yaml` 是每次从 Secret 临时生成的，**回写的 token 不会持久化**，所以每次运行都会用账密重新登录一次——这是预期行为，不报错就不用管。
</details>

<details>
<summary><b>迟到线是几点？</b></summary>

以服务器返回的 `FLateTime` 为准（现行约 **22:30**）：21:30–22:30 正常，此后签到会标记 `isLate=1`。脚本用服务器时间戳算迟到，不看 runner 的本地时钟。默认档位 21:35 / 21:50 都在迟到线之前。
</details>

<details>
<summary><b>为什么本地跑的时候直接退出了？</b></summary>

大概率是当前时间不在窗口内（脚本会打印「北京时间 xx:xx 不在签到窗口 21:00-23:59 内」）。这是设计行为：窗口外的运行一律静默跳过，避免延迟触发的补跑刷出误报。想立刻试一次就加 `CHECKIN_FORCE=1`。
</details>

<details>
<summary><b>报「服务器暂未返回今日计划」？</b></summary>

跨午夜时段（约 0:00–1:00）服务器还没发布当日计划，`init` 会返回 500，属正常现象。脚本会在 21:45 前推送一次提醒，之后的重试静默，不会轰炸你。
</details>

<details>
<summary><b>一晚会不会重复打卡？</b></summary>

不会。任意一档签成功后，当天后续每次运行都会被 `FState` 判成「已签到」直接静默退出；workflow 还有 `concurrency` 串行化，避免两个 cron 同时打同一个接口。
</details>

<details>
<summary><b>iOS 能用吗？</b></summary>

能，而且是最省事的用法：脚本跑在 GitHub 云端，iPhone 只负责收微信推送，什么都不用装。想自己抓包看报文才需要 Android 或电脑代理。
</details>

<details>
<summary><b>人不在学校会怎样？</b></summary>

脚本不做真实定位、也不伪造 GPS——这是特性也是底线。**请在本人确实在校时使用**。长期离校（实习、交换、请假）请走辅导员报备，别用工具掩盖状态。
</details>

<details>
<summary><b>大概要花多少 Actions 额度？</b></summary>

窗口内每 10 分钟扫一次、单次约几十秒，一晚上约 20 次运行（18 次窗口扫描 + 2 个窗口外提前档），公开库免费；私有库也在免费额度内（每月 2000 分钟，实际一晚只占十几分钟）。真正干活的只有到了目标时刻那一次，其余运行在 `init` 之后立刻退出。
</details>

## ⚠️ 已知限制

- 窗口**不支持跨日**（如 23:00–01:00），写了会回退默认窗口并提示。
- 自定义 `times` 必须落在 cron 扫描的时段里，否则不会被叫醒（见上文换算方法）。
- GitHub 的定时任务本身**不保证准点**，延迟 1–4 小时是常态；本项目靠「窗口扫描 cron + 十档遗留档 + 多档时刻 + 迟到补签」把漏签概率压到最低，但无法承诺 100% 准点。
- 签到完全依赖学校 H5 的接口与字段。若前端改版（接口路径、字段名、加密密钥变化），需要重新逆向；报错会明确推送，不会静默失败。
- 服务器要求验证码时 SSO 自动登录会失败，此时请手动获取 token 填进 `user.token`。

## 🤝 贡献与反馈

- 用着有问题、发现 bug、想要新功能：发邮件到 `inspring_2026@163.com`，或直接提 Issue。
- 适配其他学校的同类系统：欢迎 PR。**提交前请脱敏**：学号、密码、token、真实住址一律打码。
- 本项目不接受、不鼓励任何形式的代挂与有偿使用。

## 📄 License

[MIT](LICENSE) © 2026 fzu-auto-checkin contributors
