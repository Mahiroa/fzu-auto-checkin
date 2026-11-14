"""回归测试：验证报告里两个真实缺陷的守护用例。

BUG-1（P2）：等待结束后重新决策会重新抽时间抖动，新目标可能比刚等的那个晚出
max_wait_minutes，导致"等完即跳过"。修法是主流程复用同一批 slots，测试直接
验证 resolve_run(slots=...) 的幂等性。

观察 1：times 多档时，"下一档等不起"曾把"眼下刚过点、仍在 tolerance 内"的机会
挤成 skip。测试锁住"刚过点优先"。
"""
import datetime

from src.schedule import TZ_CN, parse_schedule_config, plan_slots, resolve_run

DAY = datetime.date(2026, 3, 2)


def at(hour: int, minute: int, second: int = 0) -> datetime.datetime:
    return datetime.datetime.combine(DAY, datetime.time(hour, minute, second), tzinfo=TZ_CN)


class UniformSeq:
    """按顺序返回固定值的假随机源（返回值语义与 random.uniform 一致：直接是结果，不是比例）。"""

    def __init__(self, values):
        self.values = list(values)
        self.calls = 0

    def uniform(self, low, high):
        self.calls += 1
        return self.values.pop(0) if self.values else 0.0

    def random(self):
        return 0.5


def _sched(**schedule):
    return parse_schedule_config({"checkin": {"schedule": schedule}})


def test_tolerance_beats_unaffordable_next_slot():
    """21:35 刚过 2 分钟（tolerance 5），下一档 21:50 差 13 分钟等不起 → 必须签。"""
    sched = _sched(times=["21:35", "21:50"], time_jitter_minutes=0)
    decision = resolve_run(sched, at(21, 37))

    assert decision.mode == "run"
    assert decision.reason == "on_target"
    assert decision.target.time() == datetime.time(21, 35)


def test_wait_then_reuse_same_slots_reaches_run():
    """等待后用同一批 slots 重判必须得 run，且 target 就是刚才等的那个时刻。"""
    sched = _sched(
        times=["21:35"],
        time_jitter_minutes=30,
        max_wait_minutes=60,
    )
    # uniform(-30, 30) 返回 24.0 → 目标 21:59
    slots = plan_slots(sched, at(21, 30), rng=UniformSeq([24.0]))

    first = resolve_run(sched, at(21, 30), slots=slots)
    assert first.mode == "wait"
    assert first.target == at(21, 59)

    second = resolve_run(sched, first.target + datetime.timedelta(seconds=1), slots=slots)
    assert second.mode == "run"
    assert second.reason == "on_target"
    assert second.target == first.target


def test_replanning_without_slots_can_lose_the_slot():
    """反证：等待后不复用 slots 会重新抽抖动，目标漂到 21:17 就只剩补签。"""
    sched = _sched(times=["21:35"], time_jitter_minutes=30, max_wait_minutes=60)
    waited = resolve_run(sched, at(21, 30), slots=plan_slots(sched, at(21, 30), rng=UniformSeq([24.0])))
    assert waited.target == at(21, 59)

    # 不传 slots：同一次调用内重新抽，uniform(-30,30) 返回 -18.0 → 21:17
    replanned = resolve_run(sched, at(21, 59, 1), rng=UniformSeq([-18.0]))
    assert replanned.target == at(21, 17)
    assert replanned.reason == "late_fallback"


def test_negative_jitter_and_window_still_guarded():
    """抖动再大也不能把目标推出窗口（重抽耗尽后贴边，in_window 恒真）。"""
    sched = _sched(times=["23:58"], time_jitter_minutes=600)
    # 每次都给出远超窗口的抖动值 → 9 次抽样全越界 → 必须走 clamped_high
    slots = plan_slots(sched, at(22, 0), rng=UniformSeq([900.0] * 12))

    assert slots
    assert slots[0].clamped is True
    assert slots[0].reason == "clamped_high"
    for slot in slots:
        assert datetime.time(21, 0) <= slot.target.time() <= datetime.time(23, 59, 59, 999999)
