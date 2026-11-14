"""签到时间调度：把"几点签"从代码搬到配置，并处理时间抖动与二次校验。

为什么单独成模块：
- 时间语义（北京时间、窗口端点、抖动落在哪一秒）一旦散在 main.py 里就没法离线复现，
  这里全部做成纯函数：不联网、不 sleep、随机源可注入，任何执行时刻都能用固定序列重放。
- GitHub Actions runner 是 UTC，签到窗口必须始终按北京时间判定，所以模块内部先做一次
  时区归一，调用方传 UTC 还是 +08:00 都不影响结论。

规格见 docs/schedule-design.md §3；对外名字与字段名由 main.py 照抄调用，勿改动。
"""

import datetime
import math
import random
from dataclasses import dataclass

DEFAULT_TIMES = ["21:35"]
DEFAULT_WINDOW_START = "21:00"
DEFAULT_WINDOW_END = "23:59"
DEFAULT_MAX_WAIT_MINUTES = 12
DEFAULT_TOLERANCE_MINUTES = 5
MAX_JITTER_REDRAWS = 8          # 抖动越界后的重抽次数
TZ_CN = datetime.timezone(datetime.timedelta(hours=8), "Asia/Shanghai")

CLAMP_JITTER_SECONDS = 60.0     # 钳制到窗口边界后再随机偏移的秒数上限，避免每次都卡在整点


# ============ 时间基础设施 ============

def now_cn() -> datetime.datetime:
    """带时区的北京当前时间。

    所有调度判断的唯一时间基准：宿主机时区（Actions 上是 UTC）不参与任何比较。
    """
    return datetime.datetime.now(TZ_CN)


def _as_cn(when: datetime.datetime) -> datetime.datetime:
    """把任意时间归一成北京时间。

    naive 时间按"已经写了北京时间"理解（调用方只会传 now_cn()，这里兜底不抛异常）；
    aware 时间统一 astimezone，防止 UTC 环境把 21:00 看成 13:00。
    """
    if when.tzinfo is None:
        return when.replace(tzinfo=TZ_CN)
    return when.astimezone(TZ_CN)


def _effective_end(end: datetime.time) -> datetime.time:
    """窗口终点的秒级延伸：整分钟写法视为"到该分钟最后一刻"。

    配置里写 23:59 的直觉含义是 23:59:59 之前还来得及，若直接跟 time(23, 59, 0)
    比较，23:59:30 会被误判越界——那样默认窗口就得写成 23:59:59，太反直觉。
    """
    if end.second == 0 and end.microsecond == 0:
        return end.replace(second=59, microsecond=999999)
    return end


def _on_day(day: datetime.date, moment: datetime.time) -> datetime.datetime:
    """把某个 time 落到指定日期上（北京时间）。"""
    return datetime.datetime.combine(day, moment, tzinfo=TZ_CN)


def _coerce_time(value):
    """time / "HH:MM" / "HH:MM:SS" → time；非法或空 → None。"""
    if isinstance(value, datetime.time):
        return value
    if isinstance(value, str):
        return parse_hhmm(value, default=None)
    return None


def _as_float(value, default: float) -> float:
    """宽松数值解析：空串、布尔、写错、NaN/Inf 一律退回 default。

    schedule 段是用户手写的 YAML，这一层不允许因为一个写错的数字让整次签到崩掉。
    """
    if value is None or isinstance(value, bool):
        return default
    if isinstance(value, str) and not value.strip():
        return default
    try:
        parsed = float(value)
    except (TypeError, ValueError):
        return default
    if not math.isfinite(parsed):
        return default
    return parsed


# ============ 配置解析 ============

def parse_hhmm(text, *, default=None) -> datetime.time:
    """解析 "21:35" / "21:35:00" / "21:35:59" → time；解析失败返回 default。

    - 容忍首尾空白与中文全角冒号（手机复制粘贴配置很常见）；
    - 不抛异常：调用方拿不到时刻就整体走默认档，比让工作流报错更划算；
    - default 允许写成字符串，免得上层还要先 parse 一次。
    """
    if isinstance(text, datetime.time):
        return text
    if isinstance(text, datetime.datetime):
        return text.time()

    fallback = _coerce_time(default) if not isinstance(default, datetime.time) else default

    if not isinstance(text, str):
        return fallback
    raw = text.strip().replace("：", ":")
    if not raw:
        return fallback

    parts = raw.split(":")
    if len(parts) not in (2, 3):
        return fallback
    try:
        hour = int(parts[0])
        minute = int(parts[1])
        second = int(parts[2]) if len(parts) == 3 else 0
    except (TypeError, ValueError):
        return fallback
    if not (0 <= hour <= 23 and 0 <= minute <= 59 and 0 <= second <= 59):
        return fallback
    return datetime.time(hour, minute, second)


def _schedule_section(cfg: dict) -> dict:
    """从配置里取出 schedule 段。

    同时接受整份配置 / checkin 段 / schedule 段本身三种形状：main.py 用哪一种都不该
    因为"少包了一层"就读到空配置。
    """
    if not isinstance(cfg, dict):
        return {}
    checkin = cfg.get("checkin")
    if isinstance(checkin, dict):
        nested = checkin.get("schedule")
        if isinstance(nested, dict):
            return nested
    direct = cfg.get("schedule")
    if isinstance(direct, dict):
        return direct
    return cfg


def _default_times() -> list:
    """解析内置默认时刻（DEFAULT_TIMES 本身也得能被解析，否则兜底同样失效）。"""
    parsed = [t for t in (_coerce_time(x) for x in DEFAULT_TIMES) if t is not None]
    return parsed or [datetime.time(21, 35)]


def _parse_times(raw) -> list:
    """times 规范化：单个字符串也接受，坏值丢弃，全空/全坏时回退 DEFAULT_TIMES。"""
    if raw is None:
        items = []
    elif isinstance(raw, str):
        items = [raw]
    elif isinstance(raw, (list, tuple, set)):
        items = list(raw)
    else:
        items = [raw]

    parsed = [t for t in (_coerce_time(x) for x in items) if t is not None]
    # times 写空或全写错时至少保住默认那一档，否则当天就一档都没有了
    return parsed or _default_times()


def parse_schedule_config(cfg: dict) -> dict:
    """把 schedule 段规范化成内部结构；任何字段写错都退回默认值而不抛异常。

    为什么集中在一处：main.py 与测试只认这个 dict 的键，默认值散落到各调用点就会变成
    "配置少一个键 → KeyError → 整次签到没跑"。
    """
    sched = _schedule_section(cfg)
    default_start = _coerce_time(DEFAULT_WINDOW_START) or datetime.time(21, 0)
    default_end = _coerce_time(DEFAULT_WINDOW_END) or datetime.time(23, 59)

    window_start = _coerce_time(sched.get("window_start")) or default_start
    window_end = _coerce_time(sched.get("window_end")) or default_end
    if window_end <= window_start:
        # 跨日窗口（如 23:00-01:00）会让"今天是哪一天"的判定变得含混，当前不支持；
        # 与其猜，不如整段回退成默认窗口，并明确告诉用户改哪里。
        print(
            "schedule 窗口配置无效：window_end {end} 不晚于 window_start {start}，"
            "已回退默认窗口 {d_start}-{d_end}（暂不支持跨日窗口）。".format(
                start=_hhmm(window_start),
                end=_hhmm(window_end),
                d_start=DEFAULT_WINDOW_START,
                d_end=DEFAULT_WINDOW_END,
            )
        )
        window_start, window_end = default_start, default_end

    return {
        "window_start": window_start,
        "window_end": window_end,
        "times": _parse_times(sched.get("times")),
        "time_jitter_minutes": max(_as_float(sched.get("time_jitter_minutes"), 0.0), 0.0),
        "max_wait_minutes": max(
            _as_float(sched.get("max_wait_minutes"), DEFAULT_MAX_WAIT_MINUTES), 0.0
        ),
        "tolerance_minutes": max(
            _as_float(sched.get("tolerance_minutes"), DEFAULT_TOLERANCE_MINUTES), 0.0
        ),
    }


def _hhmm(moment: datetime.time) -> str:
    return moment.strftime("%H:%M")


def _window_of(sched: dict) -> tuple:
    """取出窗口端点；调用方直接把原始 dict 塞进来时也不至于 KeyError。"""
    start = _coerce_time(sched.get("window_start")) or _coerce_time(DEFAULT_WINDOW_START)
    end = _coerce_time(sched.get("window_end")) or _coerce_time(DEFAULT_WINDOW_END)
    return start, end


# ============ 窗口判定 ============

def in_window(when: datetime.datetime, start: datetime.time, end: datetime.time) -> bool:
    """判断 when 的 时:分:秒 是否落在 [start, end] 内（含端点）。

    end 按整分钟收尾：配置 23:59 时 23:59:59 仍然有效、次日 00:00:00 失效。
    只比较时刻不比较日期——窗口是同日的，日期部分由调用方（plan_slots）负责。
    """
    if when is None or start is None or end is None:
        return False
    moment = _as_cn(when).time()
    return start <= moment <= _effective_end(end)


def verify_in_window(when, start, end, *, margin_seconds=0) -> datetime.datetime:
    """执行前的二次校验：在窗口内原样返回，越界抛 ValueError。

    留在这里而不放在 main.py：抖动/等待都可能把执行点推过边界，
    "是否允许签"必须只有一条判定路径，否则迟早出现两处结论不一致。
    margin_seconds 只是在窗口两端各放宽这么多秒，默认 0 = 严格按窗口。
    """
    if when is None:
        raise ValueError("打卡时间为空，无法校验签到窗口")
    if not in_window(when, start, end):
        if not (margin_seconds > 0 and _within_margin(when, start, end, margin_seconds)):
            raise ValueError(
                "当前时间 {when} 不在签到窗口 {start}-{end} 内".format(
                    when=_as_cn(when).strftime("%Y-%m-%d %H:%M:%S"),
                    start=_hhmm(start) if start else "?",
                    end=_hhmm(end) if end else "?",
                )
            )
    return when


def _within_margin(when, start, end, margin_seconds: float) -> bool:
    """窗口两端各放宽 margin_seconds 后是否命中（给等待/抖动的秒级漂移留余量）。"""
    moment = _as_cn(when)
    day = moment.date()
    low = _on_day(day, start) - datetime.timedelta(seconds=float(margin_seconds))
    high = _on_day(day, _effective_end(end)) + datetime.timedelta(seconds=float(margin_seconds))
    return low <= moment <= high


# ============ 档位规划 ============

@dataclass
class Slot:
    """一个"配置时刻"折算出的最终执行点。

    configured 与 target 分开存，是为了在日志/推送里能说清"配的是 21:35、实际按 21:41 签"，
    出问题时不必回头猜抖动抽到了多少。
    """
    configured: datetime.time       # 配置原始时刻
    target: datetime.datetime       # 抖动 + 钳制后的目标时刻（带 tz，日期取今天）
    valid: bool                     # 配置时刻本身是否落在窗口内
    jittered: bool                  # 是否发生过抖动
    clamped: bool                   # 抖动后越界被钳制到窗口边界
    reason: str = "ok"              # "ok" / "clamped_low" / "clamped_high" / "out_of_window"


def _pick_rng(rng):
    """随机源：显式注入优先，默认用 random 模块（测试可完整固定序列）。"""
    return random if rng is None else rng


def _rng_uniform(rng, low: float, high: float) -> float:
    """在 [low, high] 上取一个随机数。

    优先 rng.uniform；注入的假 rng 只实现 random() 时也能工作，
    这样测试只需固定一个序列，不必冒充整个 random 模块。
    """
    uniform = getattr(rng, "uniform", None)
    if callable(uniform):
        return float(uniform(low, high))
    return low + (high - low) * float(rng.random())


def _clamp_into_window(target, day, start, end_eff) -> datetime.datetime:
    """把钳制结果再夹一次，保证落在 [start, end_eff] 内。

    窗口特别窄（例如 start 21:00 / end 21:00）时，边界 ±60s 的随机偏移仍可能把结果
    顶出窗口；这是最后一道保险——"已钳制"的档位一旦越界就等于漏签。
    """
    return min(max(target, _on_day(day, start)), _on_day(day, end_eff))


def plan_slots(sched: dict, now: datetime.datetime, *, rng=None) -> list:
    """算出当天所有配置时刻的最终执行点（抖动 → 重抽 → 钳制）。

    三级降级的理由：抖动是为了让签到时刻看起来像人手动点的，但绝不能因为抖动把人
    抖到窗口外而漏签。所以越界先重抽，抽不回来就贴到窗口边界（再随机偏移几十秒，
    避免每天都精确卡在同一秒），最终保证 in_window(target) 恒为真。

    返回按 target 升序的列表，方便调用方直接取"最近一档"。
    """
    rng = _pick_rng(rng)
    now = _as_cn(now)
    day = now.date()
    start, end = _window_of(sched)
    end_eff = _effective_end(end)
    jitter = max(_as_float(sched.get("time_jitter_minutes"), 0.0), 0.0)

    slots = []
    for configured in sched.get("times") or _default_times():
        configured = _coerce_time(configured)
        if configured is None:
            continue
        base = _on_day(day, configured)

        # 档位本身就在窗口外：不做任何抖动，交给 resolve_run 走 no_slot_fallback
        if not in_window(base, start, end):
            slots.append(Slot(configured, base, False, False, False, "out_of_window"))
            continue

        if jitter <= 0:
            slots.append(Slot(configured, base, True, False, False, "ok"))
            continue

        target = None
        last = base
        for _ in range(MAX_JITTER_REDRAWS + 1):
            delta = _rng_uniform(rng, -jitter, jitter)
            candidate = base + datetime.timedelta(minutes=delta)
            if in_window(candidate, start, end):
                target = candidate
                break
            last = candidate

        if target is not None:
            slots.append(Slot(configured, target, True, True, False, "ok"))
            continue

        # 重抽用完仍越界：钳到窗口边界。用 last 判断偏了哪一侧（跨午夜也成立，
        # 因为比较的是完整日期时间而不是时刻）。
        if last < _on_day(day, start):
            reason = "clamped_low"
            edge = _on_day(day, start)
            target = edge + datetime.timedelta(
                seconds=_rng_uniform(rng, 0.0, CLAMP_JITTER_SECONDS)
            )
        else:
            reason = "clamped_high"
            edge = _on_day(day, end_eff)
            target = edge - datetime.timedelta(
                seconds=_rng_uniform(rng, 0.0, CLAMP_JITTER_SECONDS)
            )
        target = _clamp_into_window(target, day, start, end_eff)
        slots.append(Slot(configured, target, True, True, True, reason))

    slots.sort(key=lambda slot: slot.target)
    return slots


# ============ 决策 ============

@dataclass
class Decision:
    """一次"签 / 等 / 跳过"的结论，main.py 只按这个结构分支，不再自己算时间。"""
    mode: str                                   # "run" | "wait" | "skip"
    slot: Slot = None
    target: datetime.datetime = None
    wait_seconds: float = 0.0                   # mode == "wait" 时有效，>= 0
    reason: str = "ok"


def _guarded_target(target, now, start, end):
    """返回一个已通过 in_window 的目标时刻。

    规格要求 run/wait 的 target 一定在窗口内，这里再兜一次：上游传进来的半成品 sched
    万一算出窗口外的时刻，宁可退回"现在就签"，也不要把越界时间交给执行路径。
    """
    if target is not None and in_window(target, start, end):
        return target
    return now


def resolve_run(sched: dict, now: datetime.datetime, *, rng=None, slots=None) -> Decision:
    """决定"现在签 / 等一会儿 / 放弃"，七条分支与规格决策表逐条对应。

    顺序即优先级：
    1. 窗口外什么都不做——登录和推送都留到窗口内，避免延迟触发刷出误报；
    2. 刚过点且仍在 tolerance 内先签掉——否则"下一档还早但等不起"会把一次
       本来准点的机会挤成 skip（times=["21:35","21:50"]、21:37 到达就是这种坑）；
    3. 有未来档就看等不等得起（max_wait_minutes 是 cron 粒度 + 行动耗时的上限）；
    4. 没有未来档时用 tolerance 区分"刚过点"和"补签"——宁可迟到，也不要漏签。

    slots：外部已用 plan_slots 规划好的档位。等待（wait）之后重新决策时必须把它传回来，
    否则 plan_slots 会重新抽一次时间抖动，第二次的目标可能比刚才等的那个晚出
    max_wait_minutes，本次 cron 就白等了。传入 slots 即"按同一批目标时刻重判"。
    """
    now = _as_cn(now)
    start, end = _window_of(sched)

    if not in_window(now, start, end):
        return Decision("skip", None, None, 0.0, "out_of_window")

    if slots is None:
        slots = plan_slots(sched, now, rng=rng)
    usable = [slot for slot in slots if slot.valid]

    if not usable:
        # 所有档位都配在窗口外或解析失败：当前就在窗口内，兜底签一次也比空手而归强
        return Decision("run", None, now, 0.0, "no_slot_fallback")

    max_wait_seconds = (
        max(_as_float(sched.get("max_wait_minutes"), DEFAULT_MAX_WAIT_MINUTES), 0.0) * 60.0
    )
    tolerance_seconds = (
        max(_as_float(sched.get("tolerance_minutes"), DEFAULT_TOLERANCE_MINUTES), 0.0) * 60.0
    )

    # 刚过点、还在容忍范围内：先签这一档。把它排在 future 之前，是因为"下一档还要等
    # 超过 max_wait"时若先去判 future 就会直接 skip，明明眼下就有一档刚刚到点。
    touched = [
        slot for slot in usable
        if slot.target <= now and (now - slot.target).total_seconds() <= tolerance_seconds
    ]
    if touched:
        nearest = max(touched, key=lambda slot: slot.target)
        target = _guarded_target(nearest.target, now, start, end)
        return Decision("run", nearest, target, 0.0, "on_target")

    future = [slot for slot in usable if slot.target >= now]
    if future:
        nearest = min(future, key=lambda slot: slot.target)
        wait_seconds = (nearest.target - now).total_seconds()
        target = _guarded_target(nearest.target, now, start, end)
        if wait_seconds <= max_wait_seconds:
            # wait_seconds 不做上限截断：是否等待由上面那条 max_wait 判定决定
            return Decision("wait", nearest, target, wait_seconds, "wait_until_target")
        return Decision("skip", nearest, target, 0.0, "wait_too_long")

    latest = max(usable, key=lambda slot: slot.target)
    target = _guarded_target(latest.target, now, start, end)
    return Decision("run", latest, target, 0.0, "late_fallback")
