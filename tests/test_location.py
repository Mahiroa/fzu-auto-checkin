"""位置抖动与位置池的纯函数单测：不实例化 AttnClient、不发任何网络请求。

对应 docs/schedule-design.md 第 4 节（接口）与第 7 节（tests/test_location.py
必测项）。所有断言只吃 dict 配置与显式注入的 rng，不读真实 config.yaml。
"""
import math
import random

import pytest

from src.checkin import (
    DEFAULT_JITTER_RADIUS_M,
    _meters_per_degree,
    get_jitter_radius,
    jitter_location,
    resolve_location,
)


def _cfg(**checkin) -> dict:
    """只造最小 checkin 段：这几个函数不读 user / notify 等其他段。"""
    return {"checkin": checkin}


class _FixedRng:
    """rng 桩：把加权抽签钉在指定累计概率上，结论不依赖随机种子。"""

    def __init__(self, value: float):
        self.value = value

    def random(self) -> float:
        return self.value


@pytest.fixture
def seeded_random():
    """给 jitter_location（模块级 random）固定种子，测完还原全局状态。"""
    state = random.getstate()
    random.seed(20261114)
    try:
        yield
    finally:
        random.setstate(state)


# ============ get_jitter_radius ============

def test_get_jitter_radius_defaults_to_200():
    assert DEFAULT_JITTER_RADIUS_M == 200.0
    assert get_jitter_radius({}) == DEFAULT_JITTER_RADIUS_M
    assert get_jitter_radius({"checkin": {}}) == DEFAULT_JITTER_RADIUS_M
    assert get_jitter_radius(_cfg(longitude="119.20463")) == DEFAULT_JITTER_RADIUS_M


def test_get_jitter_radius_explicit_zero_disables_jitter():
    # 显式 0 = 关闭，不能被旧键或默认值顶回来
    assert get_jitter_radius(_cfg(location_jitter_m=0)) == 0.0
    assert get_jitter_radius(_cfg(location_jitter_m="0")) == 0.0
    assert get_jitter_radius(_cfg(location_jitter_m=0, jitter_radius_m=200)) == 0.0
    assert get_jitter_radius(_cfg(jitter_radius_m=0)) == 0.0


def test_get_jitter_radius_old_key_still_works():
    assert get_jitter_radius(_cfg(jitter_radius_m=50)) == 50.0
    assert get_jitter_radius(_cfg(jitter_radius_m="50")) == 50.0
    assert get_jitter_radius(_cfg(jitter_radius_m=12.5)) == 12.5


def test_get_jitter_radius_new_key_takes_priority():
    assert get_jitter_radius(_cfg(location_jitter_m=30, jitter_radius_m=200)) == 30.0
    assert get_jitter_radius(_cfg(location_jitter_m="120.5", jitter_radius_m="50")) == 120.5
    assert get_jitter_radius(_cfg(location_jitter_m=80, jitter_radius_m=0)) == 80.0


def test_get_jitter_radius_blank_new_key_falls_back_to_old_key():
    # config.example.yaml 里新键默认为 ""，旧配置不改文件也要按旧键抖动
    assert get_jitter_radius(_cfg(location_jitter_m="", jitter_radius_m=50)) == 50.0
    assert get_jitter_radius(_cfg(location_jitter_m="   ", jitter_radius_m="50")) == 50.0
    assert get_jitter_radius(_cfg(location_jitter_m="", jitter_radius_m=None)) == DEFAULT_JITTER_RADIUS_M
    assert get_jitter_radius(_cfg(location_jitter_m="", jitter_radius_m="")) == DEFAULT_JITTER_RADIUS_M


def test_get_jitter_radius_invalid_value_uses_default():
    assert get_jitter_radius(_cfg(location_jitter_m="两百米")) == DEFAULT_JITTER_RADIUS_M
    assert get_jitter_radius(_cfg(location_jitter_m=True)) == DEFAULT_JITTER_RADIUS_M
    assert get_jitter_radius(_cfg(jitter_radius_m=["200"])) == DEFAULT_JITTER_RADIUS_M


def test_get_jitter_radius_negative_falls_back_to_default():
    """负数属于笔误：只有显式 0 才是"关闭抖动"，-20 必须回落 200m。

    把负数当"关闭"会让一个手滑的负号静默去掉坐标抖动，且日志里看不出来——
    这类降级比直接报错更难发现，所以按写错处理。
    """
    assert get_jitter_radius(_cfg(location_jitter_m=-20)) == DEFAULT_JITTER_RADIUS_M
    assert get_jitter_radius(_cfg(location_jitter_m="-20")) == DEFAULT_JITTER_RADIUS_M
    assert get_jitter_radius(_cfg(jitter_radius_m=-5)) == DEFAULT_JITTER_RADIUS_M


# ============ resolve_location：单点路径 ============

def test_resolve_location_single_point():
    got = resolve_location(_cfg(
        longitude="119.20463",
        latitude="26.04920",
        actual_location=" 福州大学旗山校区生活区X号楼 ",
    ))
    assert set(got) == {"longitude", "latitude", "actual_location", "label"}
    assert isinstance(got["longitude"], float) and isinstance(got["latitude"], float)
    assert got["longitude"] == pytest.approx(119.20463)
    assert got["latitude"] == pytest.approx(26.04920)
    assert got["actual_location"] == "福州大学旗山校区生活区X号楼"
    assert got["label"] == "single"


def test_resolve_location_single_point_accepts_numbers():
    got = resolve_location(_cfg(longitude=119.2, latitude=26.0))
    assert (got["longitude"], got["latitude"]) == (119.2, 26.0)


def test_resolve_location_single_point_empty_actual_location():
    assert resolve_location(_cfg(longitude=119.2, latitude=26.0))["actual_location"] == ""
    assert resolve_location(_cfg(
        longitude=119.2, latitude=26.0, actual_location="   "))["actual_location"] == ""
    assert resolve_location(_cfg(
        longitude=119.2, latitude=26.0, actual_location=None))["actual_location"] == ""


@pytest.mark.parametrize("checkin", [
    {"latitude": "26.04920"},                                   # 缺经度
    {"longitude": "119.20463"},                                 # 缺纬度
    {"longitude": "", "latitude": "26.04920"},                   # 经度空字符串
    {"longitude": "119.20463", "latitude": "   "},               # 纬度只有空白
    {"longitude": "abc", "latitude": "26.04920"},                # 经度写错
    {"longitude": "119.20463", "latitude": "26.04920N"},         # 纬度带单位
    {"longitude": None, "latitude": None},
])
def test_resolve_location_bad_coordinates_raise_value_error(checkin):
    with pytest.raises(ValueError) as exc:
        resolve_location({"checkin": checkin})
    message = str(exc.value)
    # 报错必须指出改哪儿，否则用户只能去猜
    assert "checkin" in message
    assert "config.yaml" in message
    assert "longitude" in message or "latitude" in message


# ============ resolve_location：位置池路径 ============

POOL = [
    {"longitude": "119.20463", "latitude": "26.04920", "actual_location": "生活区X号楼"},
    {"longitude": "119.20510", "latitude": "26.04880", "actual_location": "教学楼"},
]


def test_resolve_location_pool_takes_priority_over_single_point():
    cfg = _cfg(longitude="1.0", latitude="2.0", actual_location="单点文案", locations=POOL)
    allowed = {(119.20463, 26.04920), (119.20510, 26.04880)}
    for value in (0.0, 0.25, 0.75, 0.999):
        got = resolve_location(cfg, rng=_FixedRng(value))
        assert (got["longitude"], got["latitude"]) in allowed
        assert got["label"] in {"locations[0]", "locations[1]"}


def test_resolve_location_pool_binds_actual_location_to_point():
    cfg = _cfg(locations=POOL, actual_location="外层文案")
    first = resolve_location(cfg, rng=_FixedRng(0.0))
    assert (first["label"], first["actual_location"]) == ("locations[0]", "生活区X号楼")
    second = resolve_location(cfg, rng=_FixedRng(0.9))
    assert (second["label"], second["actual_location"]) == ("locations[1]", "教学楼")


def test_resolve_location_pool_entry_without_text_falls_back_to_outer_text():
    cfg = _cfg(locations=[{"longitude": 119.2, "latitude": 26.0}], actual_location="外层文案")
    assert resolve_location(cfg, rng=_FixedRng(0.5))["actual_location"] == "外层文案"
    # 外层也没写 → 空串，而不是 "None"
    assert resolve_location(
        _cfg(locations=[{"longitude": 119.2, "latitude": 26.0}]), rng=_FixedRng(0.5)
    )["actual_location"] == ""


def test_resolve_location_pool_weight_missing_or_zero_counts_as_one():
    pool = [
        {"longitude": 119.0, "latitude": 26.0, "weight": 0},
        {"longitude": 119.1, "latitude": 26.1},
        {"longitude": 119.2, "latitude": 26.2, "weight": "abc"},
    ]
    cfg = _cfg(locations=pool)
    # 三项权重都被当成 1：累计概率 1/3、2/3 处应分别落到第 1、2 项
    assert resolve_location(cfg, rng=_FixedRng(0.0))["label"] == "locations[0]"
    assert resolve_location(cfg, rng=_FixedRng(0.5))["label"] == "locations[1]"
    assert resolve_location(cfg, rng=_FixedRng(0.9))["label"] == "locations[2]"


def test_resolve_location_pool_weight_biases_choice():
    pool = [
        {"longitude": 119.0, "latitude": 26.0, "weight": 9},
        {"longitude": 119.1, "latitude": 26.1, "weight": 1},
    ]
    cfg = _cfg(locations=pool)
    assert resolve_location(cfg, rng=_FixedRng(0.5))["label"] == "locations[0]"   # 0.5*10=5 < 9
    assert resolve_location(cfg, rng=_FixedRng(0.95))["label"] == "locations[1]"  # 9.5 >= 9


def test_resolve_location_pool_uses_random_from_stdlib_by_default():
    cfg = _cfg(locations=POOL)
    labels = {resolve_location(cfg)["label"] for _ in range(60)}
    # 权重相等时两点都应被抽到；60 次全落同一项的概率约 2^-59
    assert labels == {"locations[0]", "locations[1]"}


def test_resolve_location_empty_pool_falls_back_to_single_point():
    cfg = _cfg(longitude="119.20463", latitude="26.04920",
               actual_location="单点文案", locations=[])
    got = resolve_location(cfg)
    assert got["label"] == "single"
    assert got["actual_location"] == "单点文案"
    assert got["longitude"] == pytest.approx(119.20463)


def test_resolve_location_pool_bad_entry_raises_with_index():
    cfg = _cfg(locations=[POOL[0], {"longitude": "", "latitude": "26.0488"}])
    with pytest.raises(ValueError) as exc:
        # 即使 rng 只会抽到好项也要报错：整池在校验阶段就拦住
        resolve_location(cfg, rng=_FixedRng(0.0))
    message = str(exc.value)
    assert "locations[1]" in message
    assert "config.yaml" in message


def test_resolve_location_pool_ignores_broken_single_point_keys():
    # 启用了位置池后，旧单点键写错不该再拦住打卡
    cfg = _cfg(longitude="", latitude="", locations=POOL)
    assert resolve_location(cfg, rng=_FixedRng(0.0))["label"] == "locations[0]"


# ============ jitter_location 半径约束 ============

def _distance_m(lon: float, lat: float, base_lon: float, base_lat: float) -> float:
    """用模块内同一套米/度换算反算两点距离，校验抖动点确实没超出半径。"""
    m_per_lat, m_per_lon = _meters_per_degree(base_lat)
    return math.hypot((lat - base_lat) * m_per_lat, (lon - base_lon) * m_per_lon)


@pytest.mark.parametrize("radius_m", [1.0, 50.0, 200.0, 1000.0])
def test_jitter_location_stays_within_radius(seeded_random, radius_m):
    base_lon, base_lat = 119.20463, 26.04920
    distances = [
        _distance_m(*jitter_location(base_lon, base_lat, radius_m), base_lon, base_lat)
        for _ in range(300)
    ]
    # 容差 1e-6 相对误差：只留米/度展开式本身的换算误差
    assert max(distances) <= radius_m * (1 + 1e-6)
    # 同时确认点在圆内散开，而不是全挤在圆心附近
    assert max(distances) > radius_m * 0.9


def test_jitter_location_area_uniform_sampling(seeded_random):
    # r = R*sqrt(U) 才会让点按面积均匀分布：落在内半圆的点数占比应约 1/4
    base_lon, base_lat, radius_m = 119.20463, 26.04920, 200.0
    samples = 3000
    inner = sum(
        1 for _ in range(samples)
        if _distance_m(*jitter_location(base_lon, base_lat, radius_m), base_lon, base_lat)
        <= radius_m / 2
    )
    assert 0.18 <= inner / samples <= 0.32


@pytest.mark.parametrize("radius_m", [0, 0.0, -1, -200.0, None])
def test_jitter_location_zero_or_negative_returns_input(radius_m):
    assert jitter_location(119.20463, 26.04920, radius_m) == (119.20463, 26.04920)
