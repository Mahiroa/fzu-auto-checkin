"""生成 Go 服务的一致性语料（不联网、不读真实配置）。

产出两份 JSON 到 server/testdata/：

1. schedule_parity.json —— 调度器跨实现一致性：
   Python 版 `src/schedule.py` 与 Go 版 `internal/schedule` 必须对同一组
   (schedule 配置, 当前时刻, 随机序列 u[]) 得出完全相同的
   mode / reason / target / wait_seconds / slots[]。

   随机源怎么对齐：两边都不许用各自语言的默认 RNG（Mersenne Twister vs
   ChaCha8 不可能一致），而是共同消费一串 u∈[0,1)，每取一个 u 就前进一格。
   Python 侧的假 rng 只实现 random()/uniform()，并在语料里记录用到的 u 序列；
   Go 侧用 schedule.NewScriptSource(us) 以同样顺序取用。

2. aes_vectors.json —— 参数加密向量：明文 → Base64 密文（AES-128-CBC，K=IV=apexinfoapexinfo）
   与 SSO 的 AES-128-ECB 密码加密向量，用于 Go 侧逐字节比对。

用法：python scripts/gen_parity.py
"""
import base64
import contextlib
import datetime
import io
import json
import random
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from src.checkin import encrypt  # noqa: E402
from src.login import _encrypt_password  # noqa: E402
from src.schedule import parse_schedule_config, plan_slots, resolve_run  # noqa: E402

OUT = ROOT / "server" / "testdata"
TZ_CN = datetime.timezone(datetime.timedelta(hours=8), "Asia/Shanghai")

# 固定 u 序列：64 个足够覆盖「最多 8 次重抽 × 多档 + 钳制偏移」
_US = [random.Random(1234 + i).random() for i in range(64)]


class ScriptRNG:
    """循环消费 _US 的假随机源，记录消费位置，行为与 Go 的 ScriptSource 对齐。"""

    def __init__(self, us):
        self.us = list(us)
        self.cursor = 0

    def random(self) -> float:
        value = self.us[self.cursor % len(self.us)]
        self.cursor += 1
        return value

    def uniform(self, low: float, high: float) -> float:
        return low + (high - low) * self.random()


def _dt(text: str) -> datetime.datetime:
    return datetime.datetime.strptime(text, "%Y-%m-%d %H:%M:%S").replace(tzinfo=TZ_CN)


def _iso(moment):
    if moment is None:
        return None
    # Slot.configured 是 datetime.time（只有时分秒），Go 侧用 time.Time 存"今天+该时刻"，
    # 比对时统一格式化成 HH:MM:SS，避免日期部分造成假失败。
    if isinstance(moment, datetime.time):
        return moment.strftime("%H:%M:%S")
    # 目标时刻带微秒：Python timedelta 只有微秒精度、Go time.Duration 是纳秒，
    # 两边对浮点分钟取整的末位可能差 1µs，所以 Go 侧按"≤5ms 容差 + 同秒"比对。
    m = moment.astimezone(TZ_CN)
    return f"{m:%Y-%m-%dT%H:%M:%S}.{m.microsecond:06d}+08:00"


# ============ 用例矩阵 ============

SCHEDULES = [
    {},                                                   # 全默认
    {"times": ["21:35"]},
    {"times": ["21:35", "21:50"]},
    {"times": ["21:35", "21:50", "22:10"]},
    {"times": ["21:00"]},                                 # 贴在窗口起点
    {"times": ["23:59"]},                                 # 贴在窗口终点（含 :59 收尾）
    {"times": ["20:30"]},                                 # 档位在窗口外
    {"times": ["21:35"], "time_jitter_minutes": 5},
    {"times": ["21:35"], "time_jitter_minutes": 30},       # 抖动远大于窗口余量
    {"times": ["21:00"], "time_jitter_minutes": 30},       # 低端必然越界 → 钳制
    {"times": ["23:59"], "time_jitter_minutes": 30},       # 高端必然越界 → 钳制
    {"times": ["22:00"], "time_jitter_minutes": 0, "max_wait_minutes": 0},
    {"times": ["22:00"], "time_jitter_minutes": 0, "max_wait_minutes": 60},
    {"times": ["21:35"], "tolerance_minutes": 0},
    {"times": ["21:35"], "tolerance_minutes": 30},
    {"window_start": "19:00", "window_end": "20:00", "times": ["19:30"]},
    {"window_start": "23:00", "window_end": "01:00", "times": ["23:30"]},  # 跨日 → 回退默认
    {"times": []},                                        # 空列表 → 默认档
    {"times": ["21:35:59"]},                              # 秒级档
    # 脏类型（"5" / "abc"）不放进语料：Go 侧 model.ScheduleConfig 已用 *float64 定型，
    # 类型收敛由 config.Normalize 负责，放进语料只会测到 JSON 反序列化失败。
]

MOMENTS = [
    "2026-11-14 20:59:59",
    "2026-11-14 21:00:00",
    "2026-11-14 21:00:01",
    "2026-11-14 21:04:09",
    "2026-11-14 21:34:00",
    "2026-11-14 21:35:00",
    "2026-11-14 21:36:30",
    "2026-11-14 21:37:00",
    "2026-11-14 21:49:59",
    "2026-11-14 21:50:00",
    "2026-11-14 22:09:30",
    "2026-11-14 22:30:00",
    "2026-11-14 23:58:00",
    "2026-11-14 23:59:00",
    "2026-11-14 23:59:59",
    "2026-11-15 00:00:00",
    "2026-11-14 19:30:00",
]


def build_cases():
    cases = []
    for sched_index, sched in enumerate(SCHEDULES):
        for moment in MOMENTS:
            now = _dt(moment)
            cfg = {"checkin": {"schedule": dict(sched)}}
            # parse_schedule_config 会 print 跨日窗口之类的降级提示；生成语料时静音，
            # 免得 340 行一样的警告把真正的输出淹没。
            with contextlib.redirect_stdout(io.StringIO()):
                parsed = parse_schedule_config(cfg)
            rng = ScriptRNG(_US)
            slots = plan_slots(parsed, now, rng=rng)
            # 决策复用同一批 slots（与 main.py 的"一次规划、全程复用"一致）
            decision = resolve_run(parsed, now, slots=slots)
            cases.append({
                "name": f"sched{sched_index}@{moment}",
                "schedule": dict(sched),
                "now": _iso(now),
                "uses": rng.cursor,
                "expect": {
                    "mode": decision.mode,
                    "reason": decision.reason,
                    "target": _iso(decision.target),
                    "wait_seconds": round(decision.wait_seconds, 6),
                    "slots": [
                        {
                            "configured": _iso(slot.configured),
                            "target": _iso(slot.target),
                            "valid": slot.valid,
                            "jittered": slot.jittered,
                            "clamped": slot.clamped,
                            "reason": slot.reason,
                        }
                        for slot in slots
                    ],
                },
            })
    return cases


def build_aes_vectors():
    payloads = [
        {},
        {"mapType": "amap", "longitude": 119.20463, "latitude": 26.0492,
         "range": "{\"range\":[[[119.2,26.0],[119.3,26.0],[119.3,26.1]]]}"},
        {"campus": "c-1", "lon": 119.20471, "lat": 26.04912, "startTime": "21:30",
         "endTime": "23:59", "actualLocation": "福州大学旗山校区生活区3号楼",
         "caIsNo": "0", "collUnit": "u-9", "way": 1, "isLate": 0},
        {"lon": 0.1 + 0.2, "lat": -0.000001234, "note": "浮点边界"},
        {"t": "中文与 emoji ✅", "n": None, "arr": [1, 2, {"k": "v"}]},
    ]
    vectors = []
    for payload in payloads:
        vectors.append({
            "json": json.dumps(payload, ensure_ascii=False, separators=(",", ":")),
            "plain": json.dumps(payload, ensure_ascii=True, separators=(",", ":")),
            "b64": encrypt(payload),
        })

    # SSO：password 用页面下发的 croypto 做 AES-128-ECB(PKCS7)
    croypto_key = bytes(range(16))  # 固定 16 字节挑战值，方便复现
    ecb = []
    for password in ("hunter2", "学号密码Abc123!@#", ""):
        ecb.append({
            "croypto_b64": base64.b64encode(croypto_key).decode(),
            "password": password,
            "out_b64": _encrypt_password(base64.b64encode(croypto_key).decode(), password),
        })
    return {"cbc_param": vectors, "ecb_password": ecb}


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    cases = build_cases()
    target = OUT / "schedule_parity.json"
    target.write_text(
        json.dumps({"generated_by": "scripts/gen_parity.py", "u": _US, "cases": cases},
                   ensure_ascii=False, indent=1),
        encoding="utf-8",
    )
    print(f"[parity] {len(cases)} 例 -> {target.relative_to(ROOT)}")

    aes = build_aes_vectors()
    target2 = OUT / "aes_vectors.json"
    target2.write_text(json.dumps(aes, ensure_ascii=False, indent=1), encoding="utf-8")
    print(f"[aes]    {len(aes['cbc_param'])} 组 CBC + {len(aes['ecb_password'])} 组 ECB "
          f"-> {target2.relative_to(ROOT)}")

    modes = {}
    for case in cases:
        modes[case["expect"]["mode"]] = modes.get(case["expect"]["mode"], 0) + 1
    print("[parity] 决策分布:", modes)


if __name__ == "__main__":
    main()


