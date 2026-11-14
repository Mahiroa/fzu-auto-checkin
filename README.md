# fzu-auto-checkin · 福州大学晚点名自动签到

![License](https://img.shields.io/github/license/Mahiroa/fzu-auto-checkin) ![Python](https://img.shields.io/badge/python-3.11+-blue) ![GitHub Actions](https://img.shields.io/badge/cron-21%3A00%20%E2%80%93%2023%3A59%2F10min-green)

<div align="center"><img src="assets/fzu-checkin-poster.png" width="420" alt="晚点名自动签到宣传图"></div>

福州大学「智汇福大」晚点名（21:30–23:59）的自动签到工具：**每晚定时自动打卡，结果微信推送**。手机零安装，iOS / 安卓都能用。

> ⚠️ **使用须知**
>
> 晚点名是学校的安全确认制度。本项目仅用于**本人确实在校时防止漏签**，禁止用于向学校隐瞒真实在/离校状态，禁止付费代挂。违规后果由使用者自行承担。

## 功能

- ⏰ 自定义签到时刻：`schedule.times` 想写几档写几档（默认 `["21:35", "21:50"]`），签到窗口 `21:00–23:59` 由 cron 每 10 分钟叫醒一次
- 🎲 时间抖动：`schedule.time_jitter_minutes` 给每档 ±N 分钟随机偏移，抖动后钳制回窗口，不会跑到窗口外签到
- 📍 位置池：`locations` 配多个坐标按权重随机，宿舍楼比教学楼更容易被抽中；单点配置继续可用
- 📲 签到结果微信推送（Server酱 / PushPlus / Bark / 企业微信）
- 🔑 支持学号密码全自动登录，token 过期自动换新，无需手工维护
- 🎯 定位校验预检：坐标不在校区内会明确报错，不会乱点
- 🌐 坐标随机抖动：每次打卡在以选中坐标为圆心、默认 200 米半径的圆内随机取点上报，避免每晚上报同一坐标

## 快速开始（3 步）

### 第 1 步 · 拿到代码

```bash
git clone https://github.com/Mahiroa/fzu-auto-checkin.git
cd fzu-auto-checkin
pip install -r requirements.txt
```

### 第 2 步 · 填配置

> 🖱️ **不想手填配置？用[网页版配置生成器](https://ab29158412654ce793c6cea3134abfb9.app.workbuddy.link)**：填完账号 / 位置 / 推送三步后，按「有没有 GitHub 账号」二选一 —— **有账号就贴个 Token 一键部署**（自动替你建私有仓库、写入代码和你的配置），没账号跟着分步引导走。另有**补假期工具**（只选起止日期即可，还能一键写回你的仓库）和**本机记忆**（填过的内容下次打开自动带出）。纯前端页面，不收集任何数据，也不记密码 / token。也可以把仓库里的 `web/configurator.html` 下载后双击离线使用。下面的手动流程照旧可用。
>
> 📣 **想拉同学一起用？发[宣传卡](https://04a8d547cb6c4c8c91a3e31b07ea326c.app.workbuddy.link)**：一页讲清痛点 + 卖点 + 二维码，扫码直达配置页，也可以直接转发链接。仓库里的 `web/promo.html` 就是同一张卡，可离线打开。

复制模板为 `config.yaml`（此文件已被 .gitignore 忽略，不会被提交）：

```bash
cp config.example.yaml config.yaml
```

逐项填写（每项怎么填都写在模板注释里）：

| 配置项 | 填什么 | 怎么获取 |
|--------|--------|----------|
| `user.username` / `user.password` | **登录智汇福大 App 的学号和密码**（推荐，全自动免维护） | 你平时登 App 用的那套 |
| `user.token` | 免登 token（可选项，与账密二选一即可） | 见 [docs/protocol.md](docs/protocol.md) §4 |
| `checkin.longitude` / `latitude` | 你在校区内宿舍楼的坐标 | 打开[高德坐标拾取器](https://lbs.amap.com/tools/picker) → 搜索你的宿舍楼 → 复制"经度,纬度" |
| `checkin.jitter_radius_m` | 坐标随机抖动半径（米），默认 `200` | 每次打卡在选中坐标为圆心、该半径为半径的圆内随机取点上报；填 `0` 关闭 |
| `checkin.location_jitter_m` | 新键：坐标抖动半径（米），**非空时优先于** `jitter_radius_m` | 与 `jitter_radius_m` 二选一，两个键不叠加；填 `0` 关闭 |
| `checkin.locations` | 多坐标池（可选），每项一个 `longitude` / `latitude` / `actual_location`，可带 `weight` | 非空时按权重随机挑一个点，优先于上面的单点坐标 |
| `checkin.actual_location` | 打卡上报的地址文案 | 如「福州大学旗山校区生活区X号楼」 |
| `checkin.schedule.times` | 自定义签到时刻，如 `["21:35", "22:10"]` | 每个时刻都必须带引号；写几档都行，详见下文 |
| `checkin.schedule.time_jitter_minutes` | 时间抖动 ±N 分钟，`0` 关闭 | 抖动后会被二次校验钳制回 `21:00`–`23:59` |
| `notify.*` | 推送渠道（填一种即可） | [Server酱](https://sct.ftqq.com) / [PushPlus](https://www.pushplus.plus) 注册即得 key |

### 第 3 步 · 跑起来

**先本地试一次**（确认配置没问题）：

```bash
python main.py
```

输出「已签到 / 无需操作」或「签到成功」即为配置正确。

**然后挂到 GitHub Actions 云端自动跑**（推荐）：

1. 在 GitHub 上 Fork 本仓库，或新建一个 **私有仓库** 推送这份代码
2. 仓库 `Settings → Secrets and variables → Actions → New repository secret`，名称填 `CONFIG_YAML`，值 = 你本地的 `config.yaml` 全文
3. 完成。Actions 每 10 分钟扫一次北京时间 21:00–23:59（另保留原有十档 cron 兜底），到点自动签到，结果推送到微信

> 💡 为什么推荐私有仓库：你的凭据只通过 Secret 注入，不进代码库；Actions 免费额度对私有库也够用（每天跑一次绰绰有余）。

## 自定义签到时间、位置与抖动

`checkin.schedule` 整段都可以不写：缺省等价于「21:35 签到、窗口 21:00–23:59」，行为与改造前完全一致。所有时刻一律按**北京时间（UTC+8）**解释，与本机时区无关。

### schedule 各项一览

| 键 | 默认值 | 含义 |
|----|--------|------|
| `schedule.window_start` | `"21:00"` | 可行签到窗口起点（含） |
| `schedule.window_end` | `"23:59"` | 可行签到窗口终点（含，按 `23:59:59` 收尾；不支持跨日窗口） |
| `schedule.times` | `["21:35"]` | 自定义签到时刻，`HH:MM` 或 `HH:MM:SS` |
| `schedule.time_jitter_minutes` | `0` | 时间抖动 ±N 分钟，`0` 关闭 |
| `schedule.max_wait_minutes` | `12` | 早到时最多等多久，超出就放弃本次、等下一个 cron |
| `schedule.tolerance_minutes` | `5` | 迟到容忍：`now - target` 在该值内视为准点 |

### schedule.times：多档怎么写

想写几档写几档，每个时刻都必须带引号（不加引号时 YAML 按 60 进制整数解析，`21:35` 会变成 `1295`）：

```yaml
schedule:
  times: ["21:35", "22:10", "22:40"]
```

- 每个时刻是一档目标时刻，也支持 `HH:MM:SS`；留空或删掉该项按 `["21:35"]` 处理。
- 被 cron 提前叫醒时，脚本等到最近的一档；等待时长超过 `max_wait_minutes`（默认 12 分钟）就放弃本次运行，等下一个 cron 再来。
- 反过来，如果某一档**刚刚过点**、还在 `tolerance_minutes`（默认 5 分钟）之内，脚本会直接签掉这一档——即使下一档还早、等不起，也不会因为「先看未来档」被判成跳过。
- 等待（`wait`）期间用的是**同一批**目标时刻：不会睡完再重新抽一次时间抖动，导致目标漂到 20 分钟后白等一趟。
- 任意一档签到成功即为当天完成，之后每次 cron 运行的 `query_today_task` 判定「已签到」直接静默跳过，不会重复打卡。
- 落在窗口外的档（例如窗口仍是 `21:00`–`23:59` 却写了 `"20:30"`）标记为无效、不参与调度；所有档都无效时，在窗口内兜底签一次。

### 与 GitHub cron 的关系：cron 必须覆盖该时段

脚本只能「被叫醒之后决定做什么」，不能自己创造运行机会。所以 **cron 必须覆盖 `schedule.times` 所在的时段**，否则那个时刻根本不会被触发。

仓库自带的 `.github/workflows/checkin.yml` 已包含窗口扫描档（UTC 13:00–15:59 = 北京 21:00–23:59，每 10 分钟一次）：

```yaml
- cron: "*/10 13-15 * * *"
```

- 只改 `times`（仍在 `21:00`–`23:59` 内）：不动 cron，10 分钟粒度足够扫到，精确的秒级时刻由脚本自己等到点。
- 把窗口整体挪出这段（例如 `window_start: "20:00"` + `times: ["20:30"]`）：自己加对应 cron。换算规则是北京时间 = UTC + 8，北京 20:00–23:59 = UTC 12:00–15:59，所以加 `cron: "*/10 12-15 * * *"`；只加一个点也行，`cron: "30 12 * * *"` 就是北京 20:30。
- 窗口不支持跨日：`window_end` 早于或等于 `window_start` 时回退成默认 `21:00`–`23:59` 并打印提示。

### 时间抖动 ±N 分钟：抖动后一定被钳制回窗口

`time_jitter_minutes: 5` 表示每档目标时刻在 ±5 分钟内均匀随机偏移一次（填 `0` 关闭）：

```yaml
schedule:
  times: ["21:35", "22:10"]
  time_jitter_minutes: 5
```

抖动不是无约束的，抽到的时刻越出 `window_start` / `window_end` 会被拉回窗口：

- 越界先重抽，最多 8 次（`MAX_JITTER_REDRAWS`）；
- 8 次仍越界就贴边：低于 `window_start` 的落在 `window_start` 之后 0–60 秒内，高于 `window_end` 的落在 `window_end` 之前 0–60 秒内；
- 真正执行前再用 `in_window` 校验一次，窗口外一律不签。

所以不论 `time_jitter_minutes` 写多大，最终签到都落在 `21:00`–`23:59` 之内——抖动、钳制、二次校验三重保证。

### 位置抖动半径：location_jitter_m 与 jitter_radius_m 的优先级

两个键含义相同（米），只填一个就够：

| 写法 | 生效半径 |
|------|----------|
| 只填 `location_jitter_m: 300` | 300 |
| 只填 `jitter_radius_m: 50` | 50 |
| 两个都填，`location_jitter_m` 非空 | 只认 `location_jitter_m`，两键不叠加 |
| 填 `0` | 关闭抖动，按配置坐标原样上报 |
| 填负数或写错（如 `"两百米"`）、留空 | 视为笔误，回落默认 `200` 米（不是关闭——负号手滑不该静默去掉抖动） |

```yaml
checkin:
  jitter_radius_m: 200       # 旧键，继续可用
  location_jitter_m: 300     # 新键，非空即接管
```

### locations 多点池：按权重随机，优先于单点

`locations` 是列表，每项一个坐标，`actual_location` 跟着该点走：

```yaml
checkin:
  locations:
    - longitude: "119.20463"
      latitude: "26.04920"
      actual_location: "福州大学旗山校区生活区X号楼"
      weight: 3
    - longitude: "119.20510"
      latitude: "26.04880"
      actual_location: "福州大学旗山校区教学楼"
```

- 每跑一次按 `weight` 加权随机抽一个点：上面这个例子抽中宿舍楼 75%、教学楼 25%。
- `weight` 可省略，默认 `1`；写 `0` 或负数按 `1` 处理（不是「不会被选中」）。
- `locations` 非空时**优先于**单点：`checkin.longitude` / `latitude` / `actual_location` 三项全部不生效。
- 每项必须有能当数字用的 `longitude` / `latitude`，缺了的项在加载配置时被丢弃；整池都无效时启动即退出并打印写法示例，不会拿空坐标去签。
- 某一项没写 `actual_location` 时，沿用外层 `checkin.actual_location` 的文案，不会把上报名变成空串。
- 抖动以抽中的那个点为圆心，半径仍由 `location_jitter_m` / `jitter_radius_m` 决定。

### 完整的 checkin 段示例

模板 `config.example.yaml` 里 `locations` 默认是 `[]`（池子示例以注释形式给出），要用池子就换成下面这种带列表项的写法；其余键与模板逐键一致：

```yaml
checkin:
  # —— 单点位置（旧配置，继续可用；下面的 locations 非空时这三行被忽略）——
  longitude: "119.20463"
  latitude: "26.04920"
  actual_location: "福州大学旗山校区生活区X号楼"

  # —— 可选：多位置池，非空时优先于上面的单点 ——
  locations:
    - longitude: "119.20463"
      latitude: "26.04920"
      actual_location: "福州大学旗山校区生活区X号楼"
      weight: 3
    - longitude: "119.20510"
      latitude: "26.04880"
      actual_location: "福州大学旗山校区教学楼"

  # —— 位置抖动 ——
  jitter_radius_m: 200        # 旧键
  location_jitter_m: ""       # 新键，非空时优先于旧键；填 "" 表示沿用 jitter_radius_m

  # —— 时间调度 ——
  schedule:
    window_start: "21:00"     # 窗口起点（含）
    window_end: "23:59"       # 窗口终点（含，按 23:59:59 收尾）
    times: ["21:35", "21:50"] # 自定义签到时刻，想加几档加几档
    time_jitter_minutes: 0    # 时间抖动 ±N 分钟，0 关闭
    max_wait_minutes: 12      # 早到最多等 12 分钟
    tolerance_minutes: 5      # 迟到容忍 5 分钟
```

### 手动补签：窗口外强制跑一次

错过定时、人确实在校要补一次时，跳过「等到目标时刻」立即执行。

**云端补签**：GitHub 仓库 → `Actions` → `checkin` → `Run workflow` → 把 `force` 勾成 `true` → 运行。workflow 会把它落成环境变量 `CHECKIN_FORCE=1`。

**本地补签**（bash / PowerShell 二选一）：

```bash
CHECKIN_FORCE=1 python main.py
```

```powershell
$env:CHECKIN_FORCE = "1"; python main.py
```

`CHECKIN_FORCE=1` 时跳过等待、立即签到；当前时间不在 `window_start`–`window_end` 内会先打印警告，然后照常去打卡（手动补签场景）。其余行为不变：假期区间、已签到判定、失败/成功推送分支全部照旧。

## 常见问题

<details>
<summary><b>token 是什么？会过期吗？</b></summary>

登录态凭证。**推荐直接填学号密码**：脚本会自动登录换 token 并回写配置，全程无感。只有不想存密码的人才需要手动获取 token（过期后重新取一次即可）。
</details>

<details>
<summary><b>迟到线是几点？</b></summary>

**22:30**。21:30–22:30 打卡为正常，22:30–23:59 打卡记迟到。配置模板里的 `schedule.times` 是 `["21:35", "21:50"]`（代码在完全没有 `schedule` 段时缺省为 `["21:35"]`），两档都在迟到线之前；首档签成功后第二档会被 `query_today_task` 判成「已签到」静默跳过。cron 每 10 分钟扫一次 21:00–23:59，另有 20:05/20:35/21:05 提前档对冲 GitHub 定时延迟：被叫醒时若最近的档还差超过 `max_wait_minutes`（默认 12 分钟）就放弃本次、等下一个 cron 再来，落在窗口外则静默跳过。
</details>

<details>
<summary><b>运行报「服务器暂未返回今日计划」？</b></summary>

跨午夜时段（约 0:00–1:00）服务器尚未发布当日计划，会返回 500，属正常现象，稍后再试即可。定时任务设在签到窗口内，不受影响。
</details>

<details>
<summary><b>寒暑假怎么办？学期中间有不用签到的时段呢？</b></summary>

配置里的 `vacation.skip_ranges` 是**通用的**，任何人都可以按需插入自己的免签时间段（模板已预填官方校历的寒假 1.23–2.21、暑假 7.3–8.29，出处《福州大学 2026—2027 学年校历》）。学期中间加一条即可，比如离校参加比赛一周：

```yaml
vacation:
  notify: false
  skip_ranges:
    # ...已有的区间不动，往下追加：
    - name: 比赛离校
      start: "2026-11-02"
      end: "2026-11-08"
```

区间内脚本**不签、不推、不留记录**，Actions 照常跑但等于空转；区间结束自动恢复签到。新学年校历发布后更新已有区间的日期即可。注意：用 CONFIG_YAML secret 的用户直接改 secret 内容，改完无需提交任何代码。
</details>

<details>
<summary><b>坐标抖动（location_jitter_m / jitter_radius_m）是做什么的？</b></summary>

真实手机的定位每次都有几十到几百米的漂移，而固定脚本会每晚向服务器上报**一模一样**的经纬度，这个特征比坐标本身更容易被识别。开启抖动后，每次打卡会以选中坐标为圆心、在半径 `location_jitter_m`（新键，非空时优先于旧键 `jitter_radius_m`；缺省 `200`）米的圆内随机取一点上报。

- 采样是在**圆面积内均匀**取点，不是只取边缘，所以不会出现"每次都贴着圆周"的规律；
- 同一次运行里 `check`（定位校验）和 `clockIn`（提交打卡）用**同一个**随机点，与真实设备"一次定位、两处使用"一致；
- 抖动点若意外落到签到范围外，会自动重新取点（最多 3 次），仍越界则回退到配置坐标，**不会因为抖动导致漏签**；
- 填 `0` 可完全关闭；两个键都填时只认 `location_jitter_m`，不叠加。

⚠️ 抖动只改变上报坐标的形态，**不改变"人是否真的在校"这一事实**。它是让正常打卡的流量特征更接近真实设备，不是用来掩盖人不在校的。
</details>

<details>
<summary><b>人不在学校会怎样？</b></summary>

脚本不做真实定位——这是特性也是红线：**请在本人确实在校时使用**。长期离校（实习、交换等）请走辅导员报备请假，名正言顺。
</details>

<details>
<summary><b>iOS 用户能用吗？</b></summary>

能，而且是最省事的用法：脚本跑在 GitHub Actions 云端，iPhone 只收微信推送，什么都不用装。
</details>

## 工作原理

晚点名页是一个 H5 页面，认证只靠一个免登 token。本项目通过静态分析其前端代码逆向出完整协议：4 个接口（查状态 / 校时 / 定位校验 / 打卡）+ AES-CBC 参数加密，无需 Frida、无需抓包工具。完整逆向笔记见 [docs/protocol.md](docs/protocol.md)。

另附 v0.1 Android 无障碍脚本（`scripts/autojs_wandianming.js`）：模拟点击方案，不碰接口，供参考。

## 参与贡献与反馈

- **用着有问题、发现 bug、想要新功能**：欢迎发邮件到 `inspring_2026@163.com`
- 适配其他学校或系统：欢迎提 Issue / PR。提交内容请**脱敏**（密码、学号、token 一律打码）。

本项目不接受、不鼓励任何形式的代挂与有偿使用。

## License

MIT
