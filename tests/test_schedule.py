"""src/schedule.py 的离线测试：窗口端点、抖动三级降级、六条决策与配置兜底。

全部不联网、不读真实 config.yaml、不 sleep：时间与随机源都靠参数注入，
所以"21:00 整点该不该签""抖动越界怎么办"这类结论可以逐字节复现。
"""
import datetime
import inspect

import pytest

from src.schedule import (
    DEFAULT_MAX_WAIT_MINUTES,
    DEFAULT_TIMES,
    DEFAULT_TOLERANCE_MINUTES,
    DEFAULT_WINDOW_END,
    DEFAULT_WINDOW_START,
    MAX_JITTER_REDRAWS,
    TZ_CN,
    Decision,
    Slot,
    in_window,
    now_cn,
    parse_hhmm,
    parse_schedule_config,
    plan_slots,
    resolve_run,
    verify_in_window,
)

DAY = datetime.date(2025, 5, 20)
ROOT = datetime.datetime(DAY.year, DAY.month, DAY.day, tzinfo=TZ_CN)


def at(hour, minute, second=0):
    """当天（北京时间）的某个时刻。"""
    return datetime.datetime.combine(DAY, datetime.time(hour, minute, second), tzinfo=TZ_CN)


def schedule(**over):
    """一份规范化后的 sched dict，测试只覆盖关心的字段。"""
    base = {
        "window_start": datetime.time(21, 0),
        "window_end": datetime.time(23, 59),
        "times": [datetime.time(21, 35)],
        "time_jitter_minutes": 0.0,
        "max_wait_minutes": float(DEFAULT_MAX_WAIT_MINUTES),
        "tolerance_minutes": float(DEFAULT_TOLERANCE_MINUTES),
    }
    base.update(over)
    return base


class SeqRng:
    """按给定顺序吐出 uniform 值的假随机源，同时记录每次调用的区间。"""

    def __init__(self, *values):
        self.values = list(values)
        self.calls = []

    def uniform(self, low, high):
        self.calls.append((low, high))
        if not self.values:
            raise AssertionError("预设随机序列已用完：实际调用次数多于预期")
        return self.values.pop(0)


class ConstRng:
    """永远取区间端点的极值随机源，用来把抖动逼到窗口两侧。"""

    def __init__(self, mode):
        self.mode = mode
        self.calls = []

    def uniform(self, low, high):
        self.calls.append((low, high))
        return high if self.mode == "high" else low


# ============ 常量 / 字段名 / 签名（main.py 会照抄这些名字） ============

def test_constants_match_spec():
    assert DEFAULT_TIMES == ["21:35"]
    assert DEFAULT_WINDOW_START == "21:00"
    assert DEFAULT_WINDOW_END == "23:59"
    assert DEFAULT_MAX_WAIT_MINUTES == 12
    assert DEFAULT_TOLERANCE_MINUTES == 5
    assert MAX_JITTER_REDRAWS == 8
    assert TZ_CN.utcoffset(None) == datetime.timedelta(hours=8)


def test_dataclass_fields_match_spec():
    assert list(Slot.__dataclass_fields__) == [
        "configured", "target", "valid", "jittered", "clamped", "reason",
    ]
    assert list(Decision.__dataclass_fields__) == [
        "mode", "slot", "target", "wait_seconds", "reason",
    ]


def test_rng_is_keyword_only_and_defaults_to_none():
    for func in (plan_slots, resolve_run):
        rng = inspect.signature(func).parameters["rng"]
        assert rng.kind is inspect.Parameter.KEYWORD_ONLY
        assert rng.default is None
    margin = inspect.signature(verify_in_window).parameters["margin_seconds"]
    assert margin.kind is inspect.Parameter.KEYWORD_ONLY
    assert margin.default == 0


def test_now_cn_is_aware_beijing_time():
    now = now_cn()
    assert now.tzinfo is not None
    assert now.utcoffset() == datetime.timedelta(hours=8)


# ============ parse_hhmm ============

@pytest.mark.parametrize(
    "text, expected",
    [
        ("21:35", datetime.time(21, 35)),
        ("21:35:00", datetime.time(21, 35)),
        ("21:35:59", datetime.time(21, 35, 59)),
        (" 09:05 ", datetime.time(9, 5)),
        ("9:5", datetime.time(9, 5)),
        ("21：35", datetime.time(21, 35)),      # 全角冒号（手机复制粘贴的常见写法）
        (datetime.time(22, 10), datetime.time(22, 10)),
    ],
)
def test_parse_hhmm_accepts(text, expected):
    assert parse_hhmm(text) == expected


@pytest.mark.parametrize(
    "text",
    ["24:00", "21:60", "21:35:60", "abc", "", "   ", "21-35", "21:35:00:00", None, 2135],
)
def test_parse_hhmm_invalid_falls_back(text):
    assert parse_hhmm(text) is None


def test_parse_hhmm_invalid_returns_injected_default():
    assert parse_hhmm("坏值", default=datetime.time(21, 0)) == datetime.time(21, 0)
    # default 允许直接写字符串，省得上层再 parse 一次
    assert parse_hhmm("25:99", default="22:10") == datetime.time(22, 10)


# ============ parse_schedule_config ============

def test_parse_schedule_config_defaults_when_empty():
    sched = parse_schedule_config({})
    assert sched["window_start"] == datetime.time(21, 0)
    assert sched["window_end"] == datetime.time(23, 59)
    assert sched["times"] == [datetime.time(21, 35)]
    assert sched["time_jitter_minutes"] == 0.0
    assert sched["max_wait_minutes"] == float(DEFAULT_MAX_WAIT_MINUTES)
    assert sched["tolerance_minutes"] == float(DEFAULT_TOLERANCE_MINUTES)


def test_parse_schedule_config_accepts_whole_config_and_subsections():
    payload = {
        "window_start": "21:00",
        "window_end": "23:59",
        "times": ["21:35", "21:50"],
        "time_jitter_minutes": 5,
        "max_wait_minutes": 10,
        "tolerance_minutes": 3,
    }
    expected = {
        "window_start": datetime.time(21, 0),
        "window_end": datetime.time(23, 59),
        "times": [datetime.time(21, 35), datetime.time(21, 50)],
        "time_jitter_minutes": 5.0,
        "max_wait_minutes": 10.0,
        "tolerance_minutes": 3.0,
    }
    # 整份配置 / checkin 段 / schedule 段本身，三种形状都得吃下去
    assert parse_schedule_config({"checkin": {"schedule": payload}}) == expected
    assert parse_schedule_config({"schedule": payload}) == expected
    assert parse_schedule_config(payload) == expected


def test_parse_schedule_config_end_not_after_start_falls_back_and_prints(capsys):
    sched = parse_schedule_config(
        {"schedule": {"window_start": "23:00", "window_end": "01:00", "times": ["23:30"]}}
    )
    assert sched["window_start"] == datetime.time(21, 0)
    assert sched["window_end"] == datetime.time(23, 59)
    out = capsys.readouterr().out
    assert "回退默认窗口" in out
    assert "23:59" in out


def test_parse_schedule_config_equal_window_also_falls_back(capsys):
    sched = parse_schedule_config(
        {"schedule": {"window_start": "22:00", "window_end": "22:00"}}
    )
    assert (sched["window_start"], sched["window_end"]) == (
        datetime.time(21, 0), datetime.time(23, 59)
    )
    assert capsys.readouterr().out.strip() != ""


def test_parse_schedule_config_times_fallbacks():
    assert parse_schedule_config({"times": []})["times"] == [datetime.time(21, 35)]
    assert parse_schedule_config({"times": None})["times"] == [datetime.time(21, 35)]
    assert parse_schedule_config({"times": ["坏", "25:00"]})["times"] == [datetime.time(21, 35)]
    # 单个字符串也当一档
    assert parse_schedule_config({"times": "22:10"})["times"] == [datetime.time(22, 10)]
    # 坏值丢掉，好值保留
    assert parse_schedule_config({"times": ["坏", "21:50"]})["times"] == [
        datetime.time(21, 50)
    ]


def test_parse_schedule_config_numeric_fallbacks_never_raise():
    sched = parse_schedule_config(
        {
            "time_jitter_minutes": "abc",
            "max_wait_minutes": None,
            "tolerance_minutes": "坏",
        }
    )
    assert sched["time_jitter_minutes"] == 0.0
    assert sched["max_wait_minutes"] == float(DEFAULT_MAX_WAIT_MINUTES)
    assert sched["tolerance_minutes"] == float(DEFAULT_TOLERANCE_MINUTES)
    # 负数没有意义，按 0 处理（等价于关闭），但不能抛
    assert parse_schedule_config({"time_jitter_minutes": -5})["time_jitter_minutes"] == 0.0
    assert parse_schedule_config({"max_wait_minutes": "30"})["max_wait_minutes"] == 30.0


def test_parse_schedule_config_non_dict_input_uses_defaults():
    assert parse_schedule_config(None)["times"] == [datetime.time(21, 35)]
    assert parse_schedule_config([])["window_start"] == datetime.time(21, 0)


# ============ in_window：end 的 23:59:59 语义 ============

@pytest.mark.parametrize(
    "moment, expected",
    [
        (at(20, 59, 59), False),
        (at(21, 0, 0), True),
        (at(21, 0, 1), True),
        (at(23, 59, 59), True),
        (at(23, 59, 0), True),
        (at(12, 0, 0), False),
        (at(0, 0, 0), False),
        (at(0, 0, 59), False),
    ],
)
def test_in_window_default_window(moment, expected):
    assert in_window(moment, datetime.time(21, 0), datetime.time(23, 59)) is expected


def test_in_window_next_day_midnight_is_out():
    next_midnight = at(0, 0, 0) + datetime.timedelta(days=1)
    assert in_window(next_midnight, datetime.time(21, 0), datetime.time(23, 59)) is False


def test_in_window_custom_window_keeps_second_extension():
    start, end = datetime.time(22, 0), datetime.time(23, 0)
    assert in_window(at(22, 0, 0), start, end) is True
    assert in_window(at(23, 0, 30), start, end) is True     # 整分钟写法延伸到该分钟末
    assert in_window(at(23, 1, 0), start, end) is False
    assert in_window(at(21, 59, 59), start, end) is False


def test_in_window_explicit_seconds_are_respected():
    assert in_window(at(22, 30, 20), datetime.time(22, 0), datetime.time(22, 30, 10)) is False
    assert in_window(at(22, 30, 10), datetime.time(22, 0), datetime.time(22, 30, 10)) is True


def test_in_window_converts_utc_to_beijing():
    # UTC 13:00 就是北京 21:00：Actions runner 用 UTC 传进来也必须判成窗口内
    utc_now = datetime.datetime(DAY.year, DAY.month, DAY.day, 13, 0, tzinfo=datetime.timezone.utc)
    assert in_window(utc_now, datetime.time(21, 0), datetime.time(23, 59)) is True


def test_in_window_none_is_false():
    assert in_window(None, datetime.time(21, 0), datetime.time(23, 59)) is False


# ============ verify_in_window ============

def test_verify_in_window_returns_input_unchanged():
    moment = at(22, 15, 30)
    start, end = datetime.time(21, 0), datetime.time(23, 59)
    assert verify_in_window(moment, start, end) is moment


def test_verify_in_window_raises_outside():
    start, end = datetime.time(21, 0), datetime.time(23, 59)
    with pytest.raises(ValueError):
        verify_in_window(at(20, 59, 59), start, end)
    with pytest.raises(ValueError):
        verify_in_window(at(0, 0, 0), start, end)


def test_verify_in_window_margin_seconds_relaxes_both_edges():
    start, end = datetime.time(21, 0), datetime.time(23, 59)
    with pytest.raises(ValueError):
        verify_in_window(at(20, 59, 50), start, end, margin_seconds=5)
    assert verify_in_window(at(20, 59, 50), start, end, margin_seconds=15) == at(20, 59, 50)


def test_verify_in_window_none_raises():
    with pytest.raises(ValueError):
        verify_in_window(None, datetime.time(21, 0), datetime.time(23, 59))


# ============ plan_slots ============

def test_plan_slots_without_jitter_uses_configured_time():
    slots = plan_slots(schedule(), at(21, 0))
    assert len(slots) == 1
    slot = slots[0]
    assert slot.configured == datetime.time(21, 35)
    assert slot.target == at(21, 35)
    assert (slot.valid, slot.jittered, slot.clamped, slot.reason) == (True, False, False, "ok")


def test_plan_slots_sorted_by_target():
    sched = schedule(times=[datetime.time(22, 10), datetime.time(21, 35)])
    targets = [slot.target for slot in plan_slots(sched, at(21, 0))]
    assert targets == sorted(targets) == [at(21, 35), at(22, 10)]


def test_plan_slots_marks_out_of_window_slot_invalid():
    sched = schedule(times=[datetime.time(20, 30)])
    slot = plan_slots(sched, at(21, 0))[0]
    assert slot.valid is False
    assert slot.reason == "out_of_window"
    assert slot.target == at(20, 30)
    assert (slot.jittered, slot.clamped) == (False, False)


def test_plan_slots_jitter_stays_within_range_and_reports_ok():
    rng = SeqRng(12.5)
    sched = schedule(time_jitter_minutes=30.0)
    slot = plan_slots(sched, at(21, 0), rng=rng)[0]
    assert slot.target == at(21, 35) + datetime.timedelta(minutes=12.5)
    assert slot.reason == "ok"
    assert (slot.valid, slot.jittered, slot.clamped) == (True, True, False)
    assert rng.calls == [(-30.0, 30.0)]        # 抖动区间就是 ±time_jitter_minutes
    assert abs((slot.target - at(21, 35)).total_seconds()) <= 30 * 60


def test_plan_slots_redraws_until_in_window():
    rng = SeqRng(-30.0, -7.0)                  # 第一抽 20:55 越界 → 重抽，第二抽落在窗口内
    sched = schedule(times=[datetime.time(21, 25)], time_jitter_minutes=30.0)
    slot = plan_slots(sched, at(21, 0), rng=rng)[0]
    assert slot.target == at(21, 18)           # 21:25 - 7min
    assert (slot.jittered, slot.clamped, slot.reason) == (True, False, "ok")
    assert len(rng.calls) == 2


def test_plan_slots_extreme_high_rng_hits_clamped_high():
    rng = ConstRng("high")
    sched = schedule(times=[datetime.time(23, 40)], time_jitter_minutes=60.0)
    slot = plan_slots(sched, at(21, 0), rng=rng)[0]
    assert slot.reason == "clamped_high"
    assert slot.clamped is True
    assert slot.jittered is True
    assert slot.valid is True
    # 钳制结果必须真的还在窗口内，否则等于漏签
    assert in_window(slot.target, sched["window_start"], sched["window_end"]) is True
    assert at(23, 0) < slot.target <= datetime.datetime.combine(
        DAY, datetime.time(23, 59, 59), tzinfo=TZ_CN
    )
    # 9 次重抽（1 + MAX_JITTER_REDRAWS）+ 1 次钳制偏移
    assert len(rng.calls) == MAX_JITTER_REDRAWS + 2


def test_plan_slots_extreme_low_rng_hits_clamped_low():
    rng = ConstRng("low")
    sched = schedule(times=[datetime.time(21, 10)], time_jitter_minutes=60.0)
    slot = plan_slots(sched, at(21, 0), rng=rng)[0]
    assert slot.reason == "clamped_low"
    assert slot.clamped is True
    assert in_window(slot.target, sched["window_start"], sched["window_end"]) is True
    assert at(21, 0) <= slot.target < at(21, 10)
    assert len(rng.calls) == MAX_JITTER_REDRAWS + 2


def test_plan_slots_clamp_offset_is_seconds_within_a_minute():
    # 前 9 次抖动全越界（+75min），第 10 次是钳制偏移（秒）
    rng = SeqRng(*([75.0] * (MAX_JITTER_REDRAWS + 1)), 30.0)
    sched = schedule(times=[datetime.time(23, 50)], time_jitter_minutes=60.0)
    slot = plan_slots(sched, at(21, 0), rng=rng)[0]
    assert slot.reason == "clamped_high"
    edge = datetime.datetime.combine(
        DAY, datetime.time(23, 59, 59, 999999), tzinfo=TZ_CN
    )
    assert slot.target == edge - datetime.timedelta(seconds=30)
    assert rng.calls[-1] == (0.0, 60.0)


def test_plan_slots_narrow_window_clamp_still_in_window():
    # 窗口只有一分钟：边界 ±60s 的偏移理论上可以顶出窗口，必须被最后一道夹取挡住
    sched = schedule(
        window_start=datetime.time(21, 0),
        window_end=datetime.time(21, 0),
        times=[datetime.time(21, 0)],
        time_jitter_minutes=60.0,
    )
    slot = plan_slots(sched, at(21, 0), rng=ConstRng("high"))[0]
    assert slot.reason == "clamped_high"
    assert in_window(slot.target, sched["window_start"], sched["window_end"]) is True


def test_plan_slots_target_uses_today():
    slot = plan_slots(schedule(), at(21, 0), rng=SeqRng(0.0))[0]
    assert slot.target.date() == DAY
    assert slot.target.utcoffset() == datetime.timedelta(hours=8)


def test_plan_slots_falls_back_to_default_times_when_missing():
    slots = plan_slots({"window_start": datetime.time(21, 0),
                        "window_end": datetime.time(23, 59),
                        "times": []}, at(21, 0))
    assert [slot.target.time() for slot in slots] == [datetime.time(21, 35)]


def test_plan_slots_naive_now_treated_as_beijing():
    naive = datetime.datetime(DAY.year, DAY.month, DAY.day, 21, 0)
    slot = plan_slots(schedule(), naive)[0]
    assert slot.target == at(21, 35)


# ============ resolve_run：六条决策 ============

def test_resolve_run_out_of_window_before_open():
    decision = resolve_run(schedule(), at(20, 59, 59))
    assert decision.mode == "skip"
    assert decision.reason == "out_of_window"
    assert decision.slot is None
    assert decision.target is None
    assert decision.wait_seconds == 0.0


def test_resolve_run_out_of_window_after_midnight():
    decision = resolve_run(schedule(), at(0, 0, 0))
    assert (decision.mode, decision.reason) == ("skip", "out_of_window")


def test_resolve_run_opens_at_2100_with_no_valid_slot():
    # 配置时刻都在窗口外 → 窗口一开就兜底签
    decision = resolve_run(
        schedule(times=[datetime.time(20, 30)]), at(21, 0, 0)
    )
    assert decision.mode == "run"
    assert decision.reason == "no_slot_fallback"
    assert decision.slot is None
    assert decision.target == at(21, 0, 0)


def test_resolve_run_on_target_when_slot_just_passed():
    decision = resolve_run(schedule(times=[datetime.time(21, 0)]), at(21, 0, 5))
    assert decision.mode == "run"
    assert decision.reason == "on_target"
    assert decision.target == at(21, 0)
    assert decision.slot is not None and decision.slot.reason == "ok"


def test_resolve_run_runs_at_235959():
    decision = resolve_run(schedule(), at(23, 59, 59))
    assert decision.mode == "run"
    assert decision.reason == "late_fallback"
    assert decision.target == at(21, 35)


def test_resolve_run_wait_until_target():
    decision = resolve_run(
        schedule(times=[datetime.time(22, 0)]), at(21, 58, 0)
    )
    assert decision.mode == "wait"
    assert decision.reason == "wait_until_target"
    assert decision.target == at(22, 0)
    assert abs(decision.wait_seconds - 120.0) < 1e-6
    assert decision.wait_seconds >= 0


def test_resolve_run_wait_upper_bound_is_inclusive():
    # 21:00 → 21:12 正好 12 分钟，等于 max_wait，按 ≤ 判成 wait
    decision = resolve_run(schedule(times=[datetime.time(21, 12)]), at(21, 0, 0))
    assert decision.mode == "wait"
    assert decision.wait_seconds == float(DEFAULT_MAX_WAIT_MINUTES) * 60


def test_resolve_run_wait_too_long_skips():
    decision = resolve_run(
        schedule(times=[datetime.time(23, 0)], max_wait_minutes=12.0), at(21, 0, 0)
    )
    assert decision.mode == "skip"
    assert decision.reason == "wait_too_long"
    assert decision.target == at(23, 0)
    assert decision.wait_seconds == 0.0


def test_resolve_run_late_fallback():
    decision = resolve_run(schedule(times=[datetime.time(21, 30)]), at(23, 50, 0))
    assert decision.mode == "run"
    assert decision.reason == "late_fallback"
    assert decision.target == at(21, 30)


def test_resolve_run_tolerance_boundary():
    sched = schedule(times=[datetime.time(21, 35)], tolerance_minutes=5.0)
    # now - target 恰好 5 分钟 → 准点
    on_time = resolve_run(sched, at(21, 40, 0))
    assert on_time.mode == "run"
    assert on_time.reason == "on_target"
    # 多 1 秒 → 落进迟到兜底
    late = resolve_run(sched, at(21, 40, 1))
    assert late.mode == "run"
    assert late.reason == "late_fallback"


def test_resolve_run_no_slot_fallback_when_all_times_invalid():
    decision = resolve_run(
        schedule(times=[datetime.time(20, 0), datetime.time(7, 0)]), at(22, 0, 0)
    )
    assert decision.mode == "run"
    assert decision.reason == "no_slot_fallback"
    assert decision.target == at(22, 0, 0)


def test_resolve_run_target_always_passes_in_window():
    sched = schedule(times=[datetime.time(22, 0)], time_jitter_minutes=15.0)
    for now in (at(21, 0, 1), at(21, 55, 0), at(23, 59, 59), at(22, 30, 0)):
        decision = resolve_run(sched, now, rng=SeqRng(*([0.0] * 40)))
        if decision.mode in ("run", "wait"):
            assert in_window(decision.target, sched["window_start"], sched["window_end"])
        else:
            assert decision.reason in ("out_of_window", "wait_too_long")


def test_resolve_run_passes_rng_through_to_jitter():
    sched = schedule(times=[datetime.time(21, 35)], time_jitter_minutes=10.0)
    decision = resolve_run(sched, at(21, 34, 0), rng=SeqRng(5.0))
    assert decision.mode == "wait"
    assert decision.target == at(21, 40)
    assert abs(decision.wait_seconds - 360.0) < 1e-6


def test_resolve_run_accepts_utc_aware_now():
    # UTC 13:00 = 北京 21:00：窗口内同样要做出决策
    utc_now = datetime.datetime(DAY.year, DAY.month, DAY.day, 13, 0, tzinfo=datetime.timezone.utc)
    decision = resolve_run(schedule(times=[datetime.time(20, 30)]), utc_now)
    assert decision.mode == "run"
    assert decision.reason == "no_slot_fallback"
    assert decision.target.utcoffset() == datetime.timedelta(hours=8)


def test_resolve_run_accepts_naive_now_as_beijing():
    naive = datetime.datetime(DAY.year, DAY.month, DAY.day, 21, 36, 0)
    decision = resolve_run(schedule(times=[datetime.time(21, 35)]), naive)
    assert decision.mode == "run"
    assert decision.reason == "on_target"
