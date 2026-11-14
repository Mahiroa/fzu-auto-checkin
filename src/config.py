"""读取 config.yaml 配置。

加载阶段先做一次结构规范化：配置写法漂移（段类型写错、时刻不加引号被 YAML 当成
60 进制整数）如果留到 21:35 那一刻才炸，代价就是一整晚漏签 —— 所以这里能修的就地
修好并打印"该怎么改"，修不好的（比如完全没有坐标）立刻给出可照抄的写法再退出。
坐标是签到的硬前提，其余写法问题一律降级为警告 + 忽略，保证旧配置照常工作。
"""
import os
import sys

import yaml

CONFIG_PATH = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "config.yaml")

# 坐标的唯一正确来源：高德坐标拾取器（GCJ-02，与打卡上报的坐标系一致）
AMAP_PICKER = "https://lbs.amap.com/tools/picker"

# locations / schedule 里"必须能当数字用"的键：写错就丢弃该项，由下游按默认值工作，
# 免得 float() 在运行途中抛异常
_SCHEDULE_NUMERIC_KEYS = ("time_jitter_minutes", "max_wait_minutes", "tolerance_minutes")


def _warn(msg: str) -> None:
    """警告统一加前缀：出问题时一眼能分清是配置文件写法问题还是脚本自身日志。"""
    print(f"[config] 警告：{msg}")


def _coord(value):
    """配置里的经纬度 → float；留空 / 非数字返回 None。

    不在本地校验范围：坐标填到校区外应由服务端 check 返回"不在签到范围"，
    本地提前拦会把"这两个月住另一栋楼"这类正常调整也一并挡掉。
    """
    if isinstance(value, bool):   # bool 是 int 子类，true/false 不该被当成 1/0 度
        return None
    if isinstance(value, (int, float)):
        return float(value)
    if isinstance(value, str):
        text = value.strip()
        if not text:
            return None
        try:
            return float(text)
        except ValueError:
            return None
    return None


def _hhmm_from_number(value):
    """还原未加引号的 HH:MM —— YAML 1.1 会把它当 60 进制整数（21:35 → 1295）。

    文档里的时刻都带引号，但用户手抄时很少加，这里按位数还原：
    不超过 23:59 分钟数按"时:分"读，更大且不超过一天秒数按"时:分:秒"读。
    """
    if isinstance(value, bool) or not isinstance(value, int):
        return None
    if 0 <= value <= 23 * 60 + 59:
        return f"{value // 60:02d}:{value % 60:02d}"
    if 0 <= value <= 23 * 3600 + 59 * 60 + 59:
        return f"{value // 3600:02d}:{value % 3600 // 60:02d}"
    return None


def _normalize_locations(checkin: dict) -> list:
    """把 checkin.locations 收敛成"每项都有可用经纬度的字典列表"。

    位置池只是可选增强：写坏了应该退回单点坐标继续签到，而不是让整晚的签到
    因为多写一行缩进或少写个引号而失败。缺坐标的项留到运行期会让 resolve_location
    抛 ValueError，所以在加载期就剔掉并告诉用户是第几项。
    """
    raw = checkin.get("locations")
    if raw is None:
        return []
    if not isinstance(raw, list):
        _warn(
            f"checkin.locations 应该是列表（每项一个坐标点），当前写成了 {type(raw).__name__}，"
            "本次忽略该段，改用 checkin.longitude / latitude。正确写法：\n"
            "        locations:\n"
            '          - longitude: "119.20463"\n'
            '            latitude: "26.04920"\n'
            '            actual_location: "福州大学旗山校区生活区X号楼"'
        )
        return []

    good, dropped = [], []
    for idx, item in enumerate(raw, start=1):
        if (isinstance(item, dict)
                and _coord(item.get("longitude")) is not None
                and _coord(item.get("latitude")) is not None):
            good.append(item)
        else:
            dropped.append(idx)
    if dropped:
        _warn(
            "checkin.locations 第 " + "、".join(str(i) for i in dropped) + " 项没有可用经纬度"
            "（每项都要有 longitude / latitude，且是纯数字），已忽略这些项。"
            f"坐标来源：{AMAP_PICKER}"
        )
    return good


def _normalize_schedule(raw) -> dict:
    """校验 schedule 段结构：整段写错就丢掉（下游按默认 21:35 + 21:00-23:59 工作）。

    times 是最容易写歪的一项（写成字符串、时刻不加引号、写成单个时刻而不是列表）。
    这里的原则是"能还原的还原，还原不了就退回默认"——默认时刻本身是有效配置，
    降级后用户当晚照样能签到。
    """
    if raw is None:
        return {}
    if not isinstance(raw, dict):
        _warn(
            f"checkin.schedule 应该是一个映射（key: value），当前写成了 {type(raw).__name__}，"
            '本次忽略该段，按默认时刻 ["21:35"] 签到。'
        )
        return {}

    sched = dict(raw)

    if "times" in sched:
        times = sched["times"]
        if isinstance(times, str):
            # 单个字符串看起来像时刻，但类型错了：下游按列表遍历会把 "21:35" 拆成字符
            _warn(
                'checkin.schedule.times 应该是列表，例如 times: ["21:35", "22:10"]，'
                f'当前是字符串 {times!r}，本次忽略该项，按默认时刻 ["21:35"] 签到。'
            )
            sched.pop("times")
        elif isinstance(times, int) and not isinstance(times, bool):
            fixed = _hhmm_from_number(times)
            if fixed:
                _warn(
                    f"checkin.schedule.times 写成了未加引号的 {fixed}，被 YAML 当成数字 {times}；"
                    f'已按 ["{fixed}"] 修复，建议写成 times: ["{fixed}"]（务必加引号）。'
                )
                sched["times"] = [fixed]
            else:
                _warn(
                    f"checkin.schedule.times 的值 {times!r} 无法还原成时刻，"
                    '本次忽略该项，按默认时刻 ["21:35"] 签到。'
                )
                sched.pop("times")
        elif isinstance(times, list):
            items, dropped = [], []
            for item in times:
                if isinstance(item, str):
                    items.append(item)
                elif isinstance(item, int) and not isinstance(item, bool) and _hhmm_from_number(item):
                    items.append(_hhmm_from_number(item))
                    _warn(
                        f"checkin.schedule.times 里的 {item} 是未加引号的时刻（YAML 60 进制整数），"
                        f'已还原成 "{_hhmm_from_number(item)}"；建议写成 ["21:35", "22:10"] 这样带引号的形式。'
                    )
                else:
                    dropped.append(repr(item))
            if dropped:
                _warn(
                    "checkin.schedule.times 里这些值不是 HH:MM 字符串，已忽略：" + "、".join(dropped)
                    + '。正确写法：times: ["21:35", "22:10"]'
                )
            sched["times"] = items
        else:
            _warn(
                f"checkin.schedule.times 类型不对（{type(times).__name__}），"
                '本次忽略该项，按默认时刻 ["21:35"] 签到。'
            )
            sched.pop("times")

    # 窗口边界同样可能被写成未加引号的时刻，还原后再交给 schedule.parse_hhmm
    for key in ("window_start", "window_end"):
        value = sched.get(key)
        if isinstance(value, int) and not isinstance(value, bool):
            fixed = _hhmm_from_number(value)
            if fixed:
                _warn(
                    f"checkin.schedule.{key} 写成了未加引号的 {fixed}（YAML 解析成 {value}），"
                    f'已还原；建议写成 {key}: "{fixed}"。'
                )
                sched[key] = fixed
            else:
                _warn(f"checkin.schedule.{key} 的值 {value!r} 不像时刻，本次忽略该项，使用默认窗口。")
                sched.pop(key)

    # 数值项写成文字（如 time_jitter_minutes: "十分钟"）下游算不动，丢掉回到默认
    for key in _SCHEDULE_NUMERIC_KEYS:
        if key not in sched:
            continue
        value = sched[key]
        ok = not isinstance(value, bool) and isinstance(value, (int, float))
        if not ok and isinstance(value, str):
            try:
                float(value.strip())
                ok = True
            except ValueError:
                ok = False
        if not ok:
            _warn(f"checkin.schedule.{key} 应该是数字，当前是 {value!r}，已忽略该项，使用默认值。")
            sched.pop(key)

    return sched


def _normalize_checkin(cfg: dict) -> None:
    """就地规范化 checkin 段；没有可用坐标是唯一立刻退出的情形。

    退出而不是继续：没有坐标时 do_checkin 必然失败，等到 21:35 才失败只会让用户
    以为脚本跑过了。其余问题全部降级（警告 + 忽略），因为旧配置必须继续可用。
    """
    raw = cfg.get("checkin")
    if isinstance(raw, dict):
        checkin = raw
    else:
        if raw is not None:
            _warn(
                f"checkin 段应该是一个映射（key: value），当前写成了 {type(raw).__name__}，已按空段处理。"
            )
        checkin = {}
        cfg["checkin"] = checkin

    if "locations" in checkin:
        # 统一收成列表：下游只按"非空列表 → 启用位置池"判断，类型不对时自动退回单点
        checkin["locations"] = _normalize_locations(checkin)
    if "schedule" in checkin:
        checkin["schedule"] = _normalize_schedule(checkin["schedule"])

    locations = checkin.get("locations") or []
    single_ok = (_coord(checkin.get("longitude")) is not None
                 and _coord(checkin.get("latitude")) is not None)
    if locations or single_ok:
        return

    print(
        "签到位置没配好：checkin 段里既没有可用的 longitude / latitude，"
        "也没有非空的 locations 列表，无法打卡。\n"
        f"  坐标来源：高德坐标拾取器 {AMAP_PICKER}\n"
        '  搜"福州大学旗山校区"或直接点选你的宿舍楼 → 复制"经度,纬度"\n'
        "写法一 · 单点（旧配置，最小可用）：\n"
        "  checkin:\n"
        '    longitude: "119.20463"\n'
        '    latitude: "26.04920"\n'
        '    actual_location: "福州大学旗山校区生活区X号楼"\n'
        "写法二 · 位置池（多个位置随机挑一个，需要每项都有坐标）：\n"
        "  checkin:\n"
        "    locations:\n"
        '      - longitude: "119.20463"\n'
        '        latitude: "26.04920"\n'
        '        actual_location: "福州大学旗山校区生活区X号楼"\n'
        "        weight: 1\n"
        '      - longitude: "119.20510"\n'
        '        latitude: "26.04880"\n'
        '        actual_location: "福州大学旗山校区教学楼"'
    )
    for key in ("longitude", "latitude"):
        value = checkin.get(key)
        if value not in (None, "") and _coord(value) is None:
            print(f"  × 当前 {key} = {value!r} 不是数字（坐标要纯数字，用引号包起来更稳妥）")
    sys.exit(1)


def load_config(path: str = CONFIG_PATH) -> dict:
    if not os.path.exists(path):
        sys.exit("缺少 config.yaml：请先 `cp config.example.yaml config.yaml` 并填写真实值。")
    with open(path, "r", encoding="utf-8") as f:
        try:
            cfg = yaml.safe_load(f)
        except yaml.YAMLError as e:
            # YAML 语法错误是最难自查的一类：把原始报错带上，省得用户盯着缩进猜
            sys.exit(f"config.yaml 不是合法的 YAML，请按报错的位置修一下缩进/引号：\n{e}")
    if not isinstance(cfg, dict):
        sys.exit(
            "config.yaml 内容为空或顶层结构不对：顶层应该是 user / checkin / vacation / notify 这些配置段，"
            "请对照 config.example.yaml 检查缩进。"
        )

    # token 与 学号密码 二选一即可：有 token 直接用；没有则用学号密码走 SSO 登录
    user = cfg.get("user")
    if not isinstance(user, dict):
        # user 段写成列表/标量时按"未配置"处理：既别让 AttributeError 顶掉下面的友好报错，
        # 也别把原值留在 cfg 里——main.py 后面还要按 dict 读它。
        user = {}
        cfg["user"] = user
    if not (user.get("token") or (user.get("username") and user.get("password"))):
        sys.exit(
            "config.yaml 认证信息不全：请至少配置 user.token，"
            "或同时配置 user.username / user.password（智汇福大 App 登录账号）。"
        )

    _normalize_checkin(cfg)

    cfg["_config_path"] = path
    return cfg
