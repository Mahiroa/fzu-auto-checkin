"""main.py 的调度接线（规格 §5）：窗口判断、CHECKIN_FORCE，以及"不联网也能跑到底"。

为什么单独测这一层：main() 里一旦重新出现写死的 21:30/21:35 比较，
src/schedule.py 的六条决策就等于白写，而这类回归只会在真跑 Actions 的那一晚暴露。
这里全部用伪造 now + 打桩 client，把 main() 跑到"窗口判断后 return"，全程零请求。
"""
import datetime
import re
import tokenize
from pathlib import Path

import main as entry
from src.schedule import TZ_CN

ROOT = Path(__file__).resolve().parents[1]
DAY = datetime.date(2025, 5, 20)


def at(hour, minute, second=0):
    """当天的北京时间某刻；伪造 now 用，避免测试依赖真实时钟。"""
    return datetime.datetime.combine(DAY, datetime.time(hour, minute, second), tzinfo=TZ_CN)


def _cfg(**schedule) -> dict:
    """最小可用配置：带坐标（避免 load_config 的坐标校验），schedule 段按需覆盖。"""
    checkin = {"longitude": "119.20463", "latitude": "26.04920", "actual_location": "宿舍楼"}
    if schedule:
        checkin["schedule"] = schedule
    return {"user": {"token": "unit-test-token"}, "checkin": checkin}


def _code_without_comments(path: Path) -> str:
    """去掉注释后的源码。

    注释里写"旧版硬编码的 21:35 已删除"属于解释而不是实现，
    只有真正的可执行代码里出现这些时刻才算写死窗口。
    """
    with path.open("rb") as fh:
        tokens = [
            tok for tok in tokenize.tokenize(fh.readline)
            if tok.type not in (tokenize.COMMENT, tokenize.ENCODING)
        ]
    code = tokenize.untokenize(tokens)
    return code.decode("utf-8") if isinstance(code, bytes) else code


def test_entry_module_imports_offline_with_scheduler_hooks():
    # 导入本身零副作用（不读 config.yaml、不发请求）：导入失败等于整个 Action 起不来
    for name in ("main", "decide", "parse_schedule_config", "resolve_run", "verify_in_window"):
        assert hasattr(entry, name)


def test_main_has_no_hardcoded_2130_or_2135_window():
    code = _code_without_comments(ROOT / "main.py")
    assert "21:35" not in code
    assert "21:30" not in code
    # 另一种写法同样属于写死窗口
    assert not re.search(r"time\(\s*21\s*,\s*(?:30|35)\b", code)


def test_main_routes_through_scheduler_not_local_comparisons():
    code = _code_without_comments(ROOT / "main.py")
    # 窗口、时刻、抖动全部来自 schedule 模块；main 只做执行前的二次校验
    assert "parse_schedule_config(" in code
    assert "resolve_run(" in code
    assert "verify_in_window(" in code


def test_force_flag_only_accepts_literal_one(monkeypatch):
    monkeypatch.delenv("CHECKIN_FORCE", raising=False)
    assert entry._force_enabled() is False
    # CI 表达式可能落成空串或其他写法，只有严格的 "1" 才代表手动补签
    for value in ("true", "TRUE", "yes", "01", "", "1 1"):
        monkeypatch.setenv("CHECKIN_FORCE", value)
        assert entry._force_enabled() is False, value
    monkeypatch.setenv("CHECKIN_FORCE", " 1 ")
    assert entry._force_enabled() is True


def test_decide_out_of_window_is_skip(monkeypatch):
    monkeypatch.delenv("CHECKIN_FORCE", raising=False)
    decision = entry.decide(_cfg(), at(12, 0))
    assert (decision.mode, decision.reason) == ("skip", "out_of_window")


def test_decide_waits_before_target(monkeypatch):
    monkeypatch.delenv("CHECKIN_FORCE", raising=False)
    decision = entry.decide(_cfg(times=["21:30"]), at(21, 20, 0))
    assert (decision.mode, decision.reason) == ("wait", "wait_until_target")
    assert decision.wait_seconds == 600.0
    assert decision.target == at(21, 30)


def test_decide_force_turns_skip_into_immediate_run(monkeypatch):
    monkeypatch.setenv("CHECKIN_FORCE", "1")
    decision = entry.decide(_cfg(), at(12, 0))
    assert decision.mode == "run"
    # 原结论要带在 reason 里，事后才看得出"本来会跳过"
    assert decision.reason == "force:out_of_window"
    assert decision.wait_seconds == 0.0
    assert decision.target == at(12, 0)


def test_main_returns_after_window_check_without_network(monkeypatch, capsys):
    def boom(*args, **kwargs):
        raise AssertionError("窗口外不应建客户端、查询或推送")

    monkeypatch.delenv("CHECKIN_FORCE", raising=False)
    monkeypatch.setattr(entry, "load_config", lambda *a, **k: _cfg())
    monkeypatch.setattr(entry, "now_cn", lambda: at(12, 0))
    monkeypatch.setattr(entry, "AttnClient", boom)
    monkeypatch.setattr(entry, "query_today_task", boom)
    monkeypatch.setattr(entry, "do_checkin", boom)
    monkeypatch.setattr(entry, "notify", boom)

    assert entry.main() is None
    assert "不在签到窗口" in capsys.readouterr().out


def test_main_force_outside_window_warns_and_continues(monkeypatch, capsys):
    pushed = []

    monkeypatch.setenv("CHECKIN_FORCE", "1")
    monkeypatch.setattr(entry, "load_config", lambda *a, **k: _cfg())
    monkeypatch.setattr(entry, "now_cn", lambda: at(12, 0))
    monkeypatch.setattr(entry, "AttnClient", lambda token: object())
    monkeypatch.setattr(
        entry, "query_today_task",
        lambda client, cfg: {"need_checkin": False, "init": {}},
    )
    monkeypatch.setattr(entry, "do_checkin", lambda *a, **k: True)
    monkeypatch.setattr(entry, "notify", lambda *a, **k: pushed.append(a))

    entry.main()
    out = capsys.readouterr().out
    # 手动补签：窗口外只警告不拦截（规格 §5 末条），且已签到分支保持静默
    assert "CHECKIN_FORCE=1" in out
    assert "手动强制补签（窗口外）" in out
    assert "已签到" in out
    assert pushed == []
