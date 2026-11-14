# 自定义签到时间 / 位置 / 抖动 —— 实现契约（团队共享规格）

本文件是本次改造的唯一事实来源。各模块按这里的签名与语义实现，实现完成后由 lead 统一审查 + 跑测试。

## 1. 目标

1. **自定义签到时间**：用户可配置任意多个签到时刻（如 `21:35`、`22:10`），不再写死在代码里。
2. **自定义位置**：支持单坐标（兼容旧配置）与多坐标池（随机选取），`actual_location` 文案可随坐标绑定。
3. **自定义抖动**
   - 位置抖动：半径可配（米），0 = 关闭。
   - 时间抖动：±N 分钟，0 = 关闭。
4. **时间抖动后二次验证**：抖动得到的执行时刻必须落在可行签到窗口 `[window_start, window_end]`（默认 `21:00–23:59`）内；越界必须重抽 → 仍越界则钳制到窗口边界；最终执行前再校验一次。

## 2. 配置契约（config.example.yaml）

```yaml
checkin:
  # —— 单点位置（旧配置继续可用）——
  longitude: "119.20463"
  latitude: "26.04920"
  actual_location: "福州大学旗山校区生活区X号楼"

  # —— 可选：多位置池，非空时优先于上面的单点 ——
  locations:
    - longitude: "119.20463"
      latitude: "26.04920"
      actual_location: "福州大学旗山校区生活区X号楼"
      weight: 1            # 可选，默认 1，非正数按 1 处理
    - longitude: "119.20510"
      latitude: "26.04880"
      actual_location: "福州大学旗山校区教学楼"

  # —— 抖动 ——
  jitter_radius_m: 200      # 旧键：位置抖动半径（米），0 关闭；保留兼容
  location_jitter_m: ""     # 新键：位置抖动半径（米），非空时优先于 jitter_radius_m，0 关闭

  # —— 时间调度 ——
  schedule:
    window_start: "21:00"        # 可行签到窗口起点（含）
    window_end: "23:59"          # 可行签到窗口终点（含，语义到 23:59:59）
    times: ["21:35", "21:50"]    # 自定义签到时刻，HH:MM 或 HH:MM:SS；空/缺省 = ["21:35"]
    time_jitter_minutes: 0       # 时间抖动 ±N 分钟，0 关闭
    max_wait_minutes: 12         # 早到时最多等待多久（配合每 10 分钟一次的 cron）
    tolerance_minutes: 5         # 迟到容忍：now - target <= 该值视为准点
```

兼容规则：
- 缺少 `schedule`、`locations`、`location_jitter_m` 时按上面的默认值工作，行为与改造前一致。
- 所有时间一律按**北京时间**（UTC+8）解释，与运行环境的本地时区无关。

## 3. `src/schedule.py`（新文件）API

```python
DEFAULT_TIMES = ["21:35"]
DEFAULT_WINDOW_START = "21:00"
DEFAULT_WINDOW_END = "23:59"
DEFAULT_MAX_WAIT_MINUTES = 12
DEFAULT_TOLERANCE_MINUTES = 5
MAX_JITTER_REDRAWS = 8          # 抖动越界后的重抽次数
TZ_CN = datetime.timezone(datetime.timedelta(hours=8), "Asia/Shanghai")

def now_cn() -> datetime.datetime                      # 带 tz 的北京当前时间
def parse_hhmm(text, *, default=None) -> datetime.time # "21:35" / "21:35:00" → time；解析失败返回 default
def parse_schedule_config(cfg: dict) -> dict
    # 返回规范化后的 dict：
    # {"window_start": time, "window_end": time, "times": [time...],
    #  "time_jitter_minutes": float, "max_wait_minutes": float, "tolerance_minutes": float}

@dataclass
class Slot:
    configured: datetime.time       # 配置原始时刻
    target: datetime.datetime       # 抖动 + 钳制后的目标时刻（带 tz，日期取今天）
    valid: bool                     # 配置时刻本身是否落在窗口内
    jittered: bool                  # 是否发生过抖动
    clamped: bool                   # 抖动后越界被钳制到窗口边界
    reason: str                     # "ok" / "clamped_low" / "clamped_high" / "out_of_window"

def plan_slots(sched: dict, now: datetime.datetime, *, rng=None) -> list[Slot]
def in_window(when: datetime.datetime, start: datetime.time, end: datetime.time) -> bool
    # 含端点；end 的秒级延伸：in_window(t, start, end) 判定 start <= t.time() <= end
    # 比较时把 t 的 时:分:秒 与 start/end 比较（end 默认按 "23:59" → 23:59:59 收尾）
def verify_in_window(when, start, end, *, margin_seconds=0) -> datetime.datetime
    # 二次验证。在窗口内 → 原样返回；越界 → 抛 ValueError（调用方决定放弃或钳制）

@dataclass
class Decision:
    mode: str               # "run" | "wait" | "skip"
    slot: Slot | None
    target: datetime.datetime | None
    wait_seconds: float     # mode == "wait" 时有效，>= 0
    reason: str             # 见下表

def resolve_run(sched: dict, now: datetime.datetime, *, rng=None) -> Decision
```

### `plan_slots` 算法

1. 对 `times` 中每个时刻 `t`：
   - `t` 不在 `[window_start, window_end]` → `valid=False, reason="out_of_window"`，退出该档。
   - `jitter = time_jitter_minutes`；`jitter <= 0` → `target = today + t`，`jittered=False`。
   - 否则抽 `delta = U(-jitter, +jitter)` 分钟（可用 `rng` 注入以便测试）：
     - 若 `today + t + delta` 在窗口内 → 采用，`jittered=True, reason="ok"`。
     - 越界 → 重抽，最多 `MAX_JITTER_REDRAWS` 次。
     - 仍越界 → **钳制**：低于 `window_start` → `target = today + window_start + U(0,60s)`，`reason="clamped_low"`；
       高于 `window_end` → `target = today + window_end - U(0,60s)`，`reason="clamped_high"`；`clamped=True`。
2. 返回按 `target` 升序的列表。

### `resolve_run` 决策表

前置：`now` 必须带时区；只处理 `window_start <= window_end` 的同日窗口（跨日窗口不支持，`parse_schedule_config` 会把 `end <= start` 回退成默认值并打印提示）。

**按表格自上而下取第一条命中**（顺序即优先级）：

| # | 条件 | mode | reason |
|---|---|---|---|
| 1 | `now` 不在窗口内 | skip | `out_of_window` |
| 2 | 存在已过档满足 `now - target <= tolerance` | run | `on_target` |
| 3 | 存在 `target >= now` 的可用档，且 `target - now <= max_wait` | wait | `wait_until_target`（`wait_seconds = (target - now).total_seconds()`） |
| 4 | 存在 `target >= now` 的可用档，但等待超上限 | skip | `wait_too_long` |
| 5 | 无未来档、已过档也超出 tolerance，但 `now` 仍在窗口内 | run | `late_fallback`（宁可迟到不缺席） |
| 6 | `times` 全部无效（都配在窗口外/解析失败） | run | `no_slot_fallback`（窗口内兜底签） |

第 2 条排在"未来档"之前是必须的：`times=["21:35","21:50"]`、`now=21:37` 时，21:35 刚过 2 分钟（在 tolerance 内），但 21:50 还要等 13 分钟（超过 12 分钟上限）。若先判未来档就会 `skip`，明明眼下一档刚刚到点。

`resolve_run(sched, now, *, rng=None, slots=None)`：
- `slots` 传入外部已 `plan_slots` 好的档位。**等待（wait）之后重新决策必须复用同一批 slots**，否则会重新抽时间抖动，第二次的目标可能比刚才等的那个晚出 `max_wait_minutes`，本次 cron 白等一趟（回归用例 `tests/test_regressions.py::test_wait_then_reuse_same_slots_reaches_run`）。
- 返回 `run`/`wait` 时，其 `target` 必须已通过 `in_window` 校验；`wait` 的 `wait_seconds` 不做上限截断（由 `max_wait_minutes` 控制是否转为 skip）。

## 4. `src/checkin.py` 改造（位置）

新增，保留现有全部行为与对外函数签名：

```python
DEFAULT_JITTER_RADIUS_M = 200.0

def get_jitter_radius(cfg: dict) -> float
    # checkin.location_jitter_m 非空优先；否则 checkin.jitter_radius_m；都缺省 → DEFAULT_JITTER_RADIUS_M
    # 写错/负数/空字符串 → 视为默认值；显式 0 → 0（关闭抖动）

def resolve_location(cfg: dict, *, rng=None) -> dict
    # 返回 {"longitude": float, "latitude": float, "actual_location": str, "label": str}
    # 1) checkin.locations 为非空列表 → 按 weight 加权随机挑一个（weight 缺失/非法按 1）
    # 2) 否则用 checkin.longitude / latitude / actual_location（空 actual_location → ""）
    # 3) 坐标缺失或非数字 → 抛 ValueError("checkin 未配置有效坐标…")
```

`do_checkin(client, cfg, init_data)` 内部改为：
- 用 `resolve_location` 拿基准点 + 文案；
- 抖动半径用 `get_jitter_radius`；
- 其余（check 校验、越界重试 3 次、回退基准点、迟到判定、clockIn）保持不变；
- `actual_location` 使用 `resolve_location` 返回的文案。

## 5. `main.py` 集成

```
cfg = load_config()
now = now_cn()
force = (env CHECKIN_FORCE == "1")
sched = parse_schedule_config(cfg)
slots = plan_slots(sched, now)          # 只规划一次，等待前后复用
decision = decide(cfg, now, sched=sched, slots=slots)
    - skip  → 打印 reason（out_of_window / wait_too_long，均为静默、不推送）并 return
    - wait  → print 等待秒数 → time.sleep(wait+1) → decision = decide(cfg, now_cn(), slots=slots)
              最多补睡 3 轮；仍不是 run → 按 skip 处理（不推送）
    - run   → verify_in_window(now_cn(), start, end)，继续原有流程
```

保留：假期判定、token/SSO 登录、`query_today_task`、无计划/已签到/失败/成功四类推送分支。

`CHECKIN_FORCE=1`（手动补签，`workflow_dispatch` 勾 `force`）的语义只有一条：**跳过等待、跳过窗口外拦截，但仍然打印窗口警告后继续尝试签到**。补签本来就是窗口外才用得上，"窗口外直接退出"会让这个开关失去意义；CI 层不再重复一套窗口判断，只透传布尔。

## 6. `.github/workflows/checkin.yml`

- 保留现有十档 cron（对老配置兜底），**新增**窗口扫描 cron：北京时间 21:00–23:59（UTC 13:00–15:59）每 10 分钟一次 → `cron: "*/10 13-15 * * *"`。
- `workflow_dispatch` 增加输入 `force`（boolean，默认 false），透传为环境变量 `CHECKIN_FORCE`。
- job 增加 `timeout-minutes: 60`（等待流程最长约 12 分钟 + 依赖安装）。
- 注释说明：cron 必须覆盖用户 `schedule.times` 所在的时段，否则该时刻不会被触发。

## 7. 测试

`tests/` 下新增，全部离线、不联网、不读真实 config：

- `tests/test_schedule.py`
  - 窗口边界：20:59:59 → skip；21:00:00 → run（no_slot_fallback / late_fallback）；23:59:59 → run；00:00:00 → skip。
  - 时间抖动：`time_jitter_minutes=30` + 固定 `rng` → `target` 落在 ±30 分钟内；注入越界 rng → `clamped_low/high` 且 `in_window(target)` 为真。
  - `wait`：`times=["22:00"]`，now=21:58 → `mode="wait"`，`wait_seconds≈120`。
  - `wait_too_long`：now=21:00，`times=["23:00"]`，`max_wait_minutes=12` → skip。
  - `late_fallback`：now=23:50，`times=["21:30"]` → run。
  - `parse_hhmm` 非法输入回退默认；`parse_schedule_config` 对 `end <= start` 回退默认并打印。
- `tests/test_location.py`
  - `get_jitter_radius`：缺省 200、`location_jitter_m: 0` → 0、`jitter_radius_m: 50` → 50、`location_jitter_m` 优先。
  - `resolve_location`：单点、多点 weight=0/缺失、坐标为空 → ValueError。
  - `jitter_location` 半径约束：多点采样距离 <= R（容差 1e-6 相对误差）。
- `tests/test_regressions.py`（lead 加的守护用例，锁住两个真实缺陷）
  - 刚过点在 tolerance 内优先于"下一档等不起"：`times=["21:35","21:50"]`、now=21:37 → `run/on_target`。
  - 等待后复用同一批 `slots` 必然 `run`，target 不变；不传 `slots` 会重抽并把目标漂走（反证）。
  - 超大 `time_jitter_minutes` + 全越界 rng → `clamped_high`，且 `in_window(target)` 恒真。
- `tests/test_main.py` / `tests/test_assets.py`（验证阶段补的集成/资产用例）
  - `main()` 窗口外零请求零推送；`decide()` 三分支；无硬编码 21:30/21:35（去注释后检查）。
  - `config.example.yaml` 键位齐全；workflow cron / force / timeout 存在；README 无旧坏链。

## 8. 文件所有权（并行时不越界）

| 负责人 | 只允许改这些文件 |
|---|---|
| agent-schedule | `src/schedule.py`（新建）、`tests/test_schedule.py` |
| agent-location | `src/checkin.py`、`tests/test_location.py` |
| agent-config | `src/config.py`、`config.example.yaml` |
| agent-ci | `.github/workflows/checkin.yml` |
| agent-main（第二阶段） | `main.py` |
| agent-docs | `README.md`、`docs/schedule-design.md` 之外的新文档可加 |

禁止：`git commit` / `git push` / 改 `web/` 下文件 / 改动与本契约无关的既有逻辑。
