"""规格 §2 / §6 的仓库资产契约：配置模板键位、workflow cron/输入/超时、README 链接。

为什么测文件而不只测函数：模板和 cron 是用户唯一摸得到的东西。
config.example.yaml 少一个 schedule 键，用户就永远只签默认时刻；
cron 少了 `*/10 13-15 * * *`，写在 times 里的 22:40 根本不会被叫醒 ——
这些偏差函数级单测发现不了，只能对着文件本身断言。
"""
import datetime
import re
from pathlib import Path

import yaml

from src.checkin import get_jitter_radius
from src.schedule import parse_schedule_config

ROOT = Path(__file__).resolve().parents[1]
EXAMPLE = ROOT / "config.example.yaml"
WORKFLOW = ROOT / ".github" / "workflows" / "checkin.yml"
README = ROOT / "README.md"

# 规格 §6：改造前就有的十档 cron，必须对老配置继续兜底
LEGACY_CRONS = {
    "5 12 * * *", "35 12 * * *", "5 13 * * *", "35 13 * * *", "50 13 * * *",
    "5 14 * * *", "0 15 * * *", "5 15 * * *", "30 15 * * *", "55 15 * * *",
}


def _load_yaml(path: Path):
    return yaml.safe_load(path.read_text(encoding="utf-8"))


def test_config_example_is_valid_yaml_with_all_spec_keys():
    cfg = _load_yaml(EXAMPLE)
    assert isinstance(cfg, dict)
    checkin = cfg["checkin"]
    for key in (
        "longitude", "latitude", "actual_location",       # 单点（旧配置）
        "locations",                                      # 位置池
        "jitter_radius_m", "location_jitter_m",           # 位置抖动：旧键 + 新键
        "schedule",
    ):
        assert key in checkin, f"config.example.yaml 缺少 checkin.{key}"
    for key in (
        "window_start", "window_end", "times",
        "time_jitter_minutes", "max_wait_minutes", "tolerance_minutes",
    ):
        assert key in checkin["schedule"], f"config.example.yaml 缺少 checkin.schedule.{key}"


def test_config_example_schedule_values_match_spec_defaults():
    sched = parse_schedule_config(_load_yaml(EXAMPLE))
    assert sched["window_start"] == datetime.time(21, 0)
    assert sched["window_end"] == datetime.time(23, 59)
    assert sched["times"] == [datetime.time(21, 35), datetime.time(21, 50)]
    assert sched["time_jitter_minutes"] == 0.0
    assert sched["max_wait_minutes"] == 12.0
    assert sched["tolerance_minutes"] == 5.0


def test_config_example_jitter_defaults_to_200m_via_old_key():
    checkin = _load_yaml(EXAMPLE)["checkin"]
    # 模板里新键留空、旧键 200：迁移期不改文件也应保持改造前的 200m 抖动
    assert isinstance(checkin["location_jitter_m"], str) and not checkin["location_jitter_m"].strip()
    assert get_jitter_radius({"checkin": checkin}) == 200.0


def test_config_example_locations_is_a_list():
    # 单点/池子二选一：模板默认空列表（= 走单点），写成 dict 会让下游静默退化
    locations = _load_yaml(EXAMPLE)["checkin"]["locations"]
    assert isinstance(locations, list)


def test_workflow_is_parseable_and_keeps_window_scan_cron():
    text = WORKFLOW.read_text(encoding="utf-8")
    data = yaml.safe_load(text)
    assert isinstance(data, dict) and "jobs" in data
    crons = set(re.findall(r'-\s*cron:\s*"([^"]+)"', text))
    # 规格 §6：新增窗口扫描档，覆盖北京时间 21:00-23:59（UTC 13:00-15:59）
    assert "*/10 13-15 * * *" in crons
    # 十档遗留 cron 不能被扫描档替换掉
    assert LEGACY_CRONS.issubset(crons), LEGACY_CRONS - crons


def test_workflow_declares_force_input_timeout_and_env_passthrough():
    text = WORKFLOW.read_text(encoding="utf-8")
    assert "workflow_dispatch:" in text
    # force 必须是 boolean + 默认 false：字符串输入会让表达式判断失真
    assert re.search(r"force:\s*\n\s*type:\s*boolean\s*\n\s*default:\s*false", text)
    assert re.search(r"timeout-minutes:\s*60", text)
    # 透传成 main.py 唯一认的环境变量
    assert "CHECKIN_FORCE:" in text


def test_readme_has_no_stale_account_link():
    readme = README.read_text(encoding="utf-8")
    # 旧账号（改名前的仓库地址）会让 clone / 徽章指向 404
    assert "cccccheyu" not in readme
    assert re.search(r"https://github\.com/[\w.-]+/fzu-auto-checkin\.git", readme)
