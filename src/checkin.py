"""晚点名签到客户端（v1.0 · 接口版）。

协议逆向自智汇福大 App 晚点名 H5（yzsxg.fzu.edu.cn），完整协议见 docs/protocol.md。

要点：
- 认证：H5 页面 URL 的 `?token=xxx`（App 免登下发）→ 请求头 `token: <token>`
- 加密：AES-128-CBC，KEY = IV = "apexinfoapexinfo"，PKCS7，输出 Base64
- check / clockIn / currentTimestamp 为 GET，密文放查询参数 `param=<urlencode(密文)>`
- init 为 POST，明文 JSON
"""

import base64
import datetime
import json
import math
import random
import urllib.parse

import requests
from Crypto.Cipher import AES
from Crypto.Util.Padding import pad, unpad

BASE_URL = "https://yzsxg.fzu.edu.cn"
API_PREFIX = "/livecloud/project/fzu/attn"
AES_KEY = b"apexinfoapexinfo"  # KEY == IV（前端源码硬编码）

# 打卡坐标随机抖动：默认半径 200 米。checkin.location_jitter_m（新键）优先于
# checkin.jitter_radius_m（旧键，保留兼容），两键任一填 0 都表示关闭抖动
DEFAULT_JITTER_RADIUS_M = 200.0
JITTER_MAX_ATTEMPTS = 3  # 抖动点意外落在签到范围外时的重试次数

# GitHub Actions 跑在 UTC，迟到判定必须按北京时间算，不能用运行环境本地时间
TZ_CN = datetime.timezone(datetime.timedelta(hours=8), "Asia/Shanghai")


def beijing_now(ts_ms=None) -> datetime.datetime:
    """服务器时间戳(毫秒) → 北京时间；没有则取当前北京时间。"""
    if ts_ms:
        return datetime.datetime.fromtimestamp(ts_ms / 1000, TZ_CN)
    return datetime.datetime.now(TZ_CN)

UA = (
    "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 "
    "Chrome/120 Mobile Safari/537.36"
)


# ============ 加解密 ============

def encrypt(obj) -> str:
    """AES-128-CBC(key=iv) PKCS7 → Base64，与前端 Encrypt() 一致。"""
    data = obj if isinstance(obj, str) else json.dumps(obj, separators=(",", ":"))
    cipher = AES.new(AES_KEY, AES.MODE_CBC, iv=AES_KEY)
    return base64.b64encode(cipher.encrypt(pad(data.encode(), AES.block_size))).decode()


def decrypt(b64text: str) -> str:
    """与前端 Decrypt() 一致：Base64 → AES-128-CBC 解密。"""
    cipher = AES.new(AES_KEY, AES.MODE_CBC, iv=AES_KEY)
    return unpad(cipher.decrypt(base64.b64decode(b64text)), AES.block_size).decode()


# ============ 打卡坐标抖动 ============

def _meters_per_degree(latitude: float):
    """该纬度下 1° 纬度 / 1° 经度对应的米数（WGS-84 椭球近似展开式）。

    200m 量级下精度远优于实际需要，仅用于把"米"换算成经纬度增量。
    """
    phi = math.radians(latitude)
    m_per_lat = 111132.92 - 559.82 * math.cos(2 * phi) + 1.175 * math.cos(4 * phi)
    m_per_lon = 111412.84 * math.cos(phi) - 93.5 * math.cos(3 * phi)
    return m_per_lat, max(m_per_lon, 1e-9)   # 近极点处兜底，避免除零


def jitter_location(longitude: float, latitude: float, radius_m: float):
    """在以 (longitude, latitude) 为圆心、radius_m 米为半径的圆内均匀随机取一点。

    - 半径按面积均匀采样：r = R * sqrt(U)，避免随机点过度聚集在圆心；
    - 方位角 θ 在 [0, 2π) 均匀分布；
    - radius_m <= 0 时原样返回入参（不抖动）。

    返回 (longitude, latitude)。每次调用结果都不同。
    """
    if not radius_m or radius_m <= 0:
        return longitude, latitude
    r = radius_m * math.sqrt(random.random())
    theta = random.uniform(0.0, 2.0 * math.pi)
    north_m = r * math.cos(theta)
    east_m = r * math.sin(theta)
    m_per_lat, m_per_lon = _meters_per_degree(latitude)
    return longitude + east_m / m_per_lon, latitude + north_m / m_per_lat


# ============ 位置配置解析 ============

def _coordinate_error(where: str, value) -> ValueError:
    """构造坐标报错：必须写清"改哪个键、改成什么样"，否则用户只能去猜。"""
    shown = "空（未配置）" if value is None or (
        isinstance(value, str) and not value.strip()
    ) else repr(value)
    return ValueError(
        f"checkin 未配置有效坐标：{where} 的值是{shown}。"
        "请修改 config.yaml 的 checkin 段，填高德坐标拾取器"
        "（https://lbs.amap.com/tools/picker）复制来的数字，"
        "如 longitude: \"119.20463\" / latitude: \"26.04920\"；"
        "也可以改用 checkin.locations 位置池，在每一项里分别填写 longitude / latitude。"
    )


def _parse_coordinate(value, where: str) -> float:
    """配置里的经纬度 → float；缺失、空白、非数字一律抛 ValueError。

    为什么不直接 float(value)：坐标是从坐标拾取器手抄进 YAML 的，最常见错法
    是留空或粘进多余字符。这里要把"哪个键空了/写错了"讲明白，而不是抛一句
    could not convert string to float 让人从头排。
    """
    if value is None or isinstance(value, bool):
        raise _coordinate_error(where, value)
    if isinstance(value, str):
        text = value.strip()
        if not text:
            raise _coordinate_error(where, value)
        value = text
    try:
        return float(value)
    except (TypeError, ValueError):
        raise _coordinate_error(where, value) from None


CAMPUS_TAG = "旗山校区"
NEAR_SEP = "靠近"
_ADMIN_HEADS = ("福建省", "福州市", "闽侯县")
_MERGE_MIN = 4          # 两段首尾重叠少于 4 字不当作同一段（"学园路" + "路28号" 只重 1 字）
_DUP_MIN = 3            # 相邻完全重复少于 3 字不折叠（"上街镇" + "镇上" 是正常连读）
_INVISIBLE = str.maketrans({
    "\u200b": " ", "\ufeff": " ",   # 零宽字符：从网页粘贴过来时最常见
    "\u00a0": " ", "\u3000": " ",   # 不换行空格 / 全角空格
})


def _normalize_actual_location(raw) -> str:
    """把上报地点文案收口成「校区 + 详细地址 + 靠近 POI」的格式。

    格式（半角空格分隔，与 server/internal/model/address.go 同一套规则）::

        旗山校区 福建省福州市闽侯县学府南路辅路28号靠近福州大学旗山校区生活区X号楼
        └校区标记┘ └─ 省/市/县 + 道路门牌 ─┘└── 靠近 + POI 全称 ──┘

    为什么需要它：actualLocation 由三处产出——地图选点自动生成、手抄、位置池逐项
    写。高德逆地理编码已经在 formatted_address 里带出楼栋名时，前端再按"靠近POI"
    追加一遍同一栋楼，就成"…生活区X号楼靠近…生活区X号楼"：同一处说了
    两遍。只有这种重复才合并；正常的"门牌 → 楼栋"两段原样保留。

    规则：空白折叠 → 摘/补"旗山校区"标记 → "靠近X"按包含/重叠关系合并（不重复时
    保留）→ 相邻重复段折叠。幂等：已经规范的文案再归一一次逐字不变。
    """
    text = " ".join(str(raw).translate(_INVISIBLE).split())
    if not text:
        return ""
    tagged = text.startswith(CAMPUS_TAG)
    body = text[len(CAMPUS_TAG):].strip() if tagged else text
    if body.startswith(CAMPUS_TAG):     # "旗山校区 旗山校区生活区X号楼…"：标记重复
        body = body[len(CAMPUS_TAG):].strip()
    body = _collapse_adjacent_repeat(_merge_near(body)).strip()
    if not body:
        return CAMPUS_TAG if tagged else ""
    if tagged or body.startswith(_ADMIN_HEADS):
        return CAMPUS_TAG + " " + body
    return body


def _merge_near(text: str) -> str:
    """把 "…靠近X" 并进主地址；X 已包含在主地址里时整段丢掉。"""
    parts = text.split(NEAR_SEP)
    if len(parts) < 2:
        return text
    merged = parts[0].strip()
    for part in parts[1:]:
        merged = _merge_head_tail(merged, part.strip())
    return merged


def _merge_head_tail(head: str, tail: str) -> str:
    """合并两段地址；能合并时结果里不留连接词，合并不了才把"靠近"补回去。"""
    if not tail:
        return head
    if not head:
        return tail
    if tail in head:            # 主地址已经写到这栋楼：靠近子句是纯重复
        return head
    if head in tail:            # 反过来被 POI 名包含：用更完整的那段
        return tail
    for k in range(min(len(head), len(tail)), _MERGE_MIN - 1, -1):
        if head[-k:] == tail[:k]:   # 首尾重叠：在重叠处拼接
            return head + tail[k:]
    return head + NEAR_SEP + tail


def _collapse_adjacent_repeat(text: str) -> str:
    """相邻两段逐字相同（"XX"）只留一段，循环到稳定为止。"""
    while True:
        cut = None
        for i in range(len(text)):
            for k in range((len(text) - i) // 2, _DUP_MIN - 1, -1):
                if text[i:i + k] == text[i + k:i + 2 * k]:
                    cut = (i, k)
                    break
            if cut:
                break
        if cut is None:
            return text
        i, k = cut
        text = text[:i + k] + text[i + 2 * k:]


def _actual_location_text(value, fallback: str = "") -> str:
    """地址文案收口成「校区 + 详细地址 + 靠近 POI」；空/缺失用 fallback。

    actualLocation 原样提交给学校接口，None 或带换行的多行 YAML 值都会让
    接口校验失败，所以统一在这里归一（空白折叠 + 去重，见
    _normalize_actual_location）。
    """
    if value is None:
        return fallback
    text = _normalize_actual_location(value)
    return text or fallback


def _location_weight(value) -> float:
    """位置池权重：缺失/非法/<=0 一律按 1。

    权重只是"偏好"而不是硬约束，一处笔误不该让某个点永远抽不到（<=0 若当真
    会导致 total 为 0 除零），所以统一退回等权。
    """
    if value is None or isinstance(value, bool):
        return 1.0
    if isinstance(value, str):
        if not value.strip():
            return 1.0
        value = value.strip()
    try:
        weight = float(value)
    except (TypeError, ValueError):
        return 1.0
    return weight if weight > 0 else 1.0


def get_jitter_radius(cfg: dict) -> float:
    """本次打卡的位置抖动半径（米），0 表示不抖动。

    新键 checkin.location_jitter_m 优先于旧键 checkin.jitter_radius_m：老配置
    只有旧键，不改文件就能继续按原半径抖动；新模板里新键默认留空，留空视同
    未配置、继续看旧键，都没有才落到默认 200m。显式 0 是用户明确要关掉抖动，
    不能因为"0 看起来跟没填一样"又悄悄恢复成 200m；负数与写错的值一样属于
    笔误，按默认处理——把 -200 当成"关闭抖动"会让一个手滑的负号静默去掉坐标
    抖动，而这恰恰是最不容易被发现的降级。
    """
    checkin = cfg.get("checkin") or {}
    for key in ("location_jitter_m", "jitter_radius_m"):
        if key not in checkin:
            continue
        raw = checkin[key]
        if raw is None:
            continue
        if isinstance(raw, str) and not raw.strip():
            continue                      # 留空 = 未配置，继续看下一个键
        if isinstance(raw, bool):
            return DEFAULT_JITTER_RADIUS_M
        try:
            radius_m = float(raw)
        except (TypeError, ValueError):
            return DEFAULT_JITTER_RADIUS_M
        if radius_m < 0:
            print(
                f"checkin.{key} 写成了负数（{raw!r}），已按默认 {DEFAULT_JITTER_RADIUS_M:.0f}m 处理；"
                "关闭位置抖动请显式写 0。"
            )
            return DEFAULT_JITTER_RADIUS_M
        return radius_m
    return DEFAULT_JITTER_RADIUS_M


def resolve_location(cfg: dict, *, rng=None) -> dict:
    """解析本次上报的位置，返回 {longitude, latitude, actual_location, label}。

    为什么需要位置池：固定坐标每晚分毫不差，比抖动更像脚本。配了
    checkin.locations 后每跑一次按 weight 从池里抽一个点，连 actualLocation
    文案一起换，上报特征更接近真人。rng 只为测试注入（random.Random(seed)
    或任意带 .random() 的对象），生产路径用标准库 random。
    """
    checkin = cfg.get("checkin") or {}
    pool = checkin.get("locations")
    if isinstance(pool, list) and pool:
        return _location_from_pool(pool, checkin, rng=rng)
    return _location_from_single(checkin)


def _location_from_single(checkin: dict) -> dict:
    """旧配置路径：checkin.longitude / latitude / actual_location 单点。"""
    return {
        "longitude": _parse_coordinate(checkin.get("longitude"), "checkin.longitude"),
        "latitude": _parse_coordinate(checkin.get("latitude"), "checkin.latitude"),
        "actual_location": _actual_location_text(checkin.get("actual_location")),
        "label": "single",
    }


def _location_from_pool(pool: list, checkin: dict, *, rng=None) -> dict:
    """位置池路径：先整池解析校验，再按 weight 加权抽一个点。

    整池预校验而不是只校验抽中的那项：坐标抄错时应当立刻报错并指出第几项，
    而不是某个随机值恰好抽到才偶发失败——那种 bug 排查起来像幽灵。
    """
    candidates = []
    for index, item in enumerate(pool):
        where = f"checkin.locations[{index}]"
        if not isinstance(item, dict):
            raise ValueError(
                f"checkin 未配置有效坐标：{where} 不是键值对。请照 config.example.yaml "
                "里 checkin.locations 的示例，在 config.yaml 中写成 "
                "- longitude / latitude / actual_location / weight 这样的子项。"
            )
        candidates.append({
            "index": index,
            "longitude": _parse_coordinate(item.get("longitude"), f"{where}.longitude"),
            "latitude": _parse_coordinate(item.get("latitude"), f"{where}.latitude"),
            "weight": _location_weight(item.get("weight")),
            "actual_location": item.get("actual_location"),
        })

    chooser = rng if rng is not None else random
    total = sum(c["weight"] for c in candidates)   # weight 已保证 > 0，total > 0
    # 加权抽签：U(0, total) 落在哪一段就取哪个点，权重越大区间越宽
    point = chooser.random() * total
    chosen = candidates[-1]
    cursor = 0.0
    for candidate in candidates:
        cursor += candidate["weight"]
        if point < cursor:
            chosen = candidate
            break

    # 池内某项没写 actual_location 时沿用外层文案，避免上报名突然变成空串
    default_text = _actual_location_text(checkin.get("actual_location"))
    return {
        "longitude": chosen["longitude"],
        "latitude": chosen["latitude"],
        "actual_location": _actual_location_text(chosen["actual_location"], default_text),
        "label": f"locations[{chosen['index']}]",
    }


# ============ 客户端 ============

class AttnClient:
    """晚点名接口客户端，token 来自 App 免登 URL（获取方式见 docs/protocol.md）。"""

    def __init__(self, token: str):
        self.token = token
        self.http = requests.Session()
        self.http.headers.update({"User-Agent": UA, "token": token})

    def _enc_get(self, path: str, payload: dict):
        """GET，密文放 param 查询参数（对应前端 createEncryptApi）。

        注意：前端 axios 会对已 encodeURIComponent 的值再编码一次（双重编码），
        服务端自行 URLDecoder 解一层，这里保持一致。
        """
        param = urllib.parse.quote(urllib.parse.quote(encrypt(payload), safe=""), safe="")
        url = f"{BASE_URL}{API_PREFIX}/{path}?param={param}"
        resp = self.http.get(url, timeout=15)
        resp.raise_for_status()
        body = resp.json()
        if not (body.get("success") and body.get("code") == 1):
            raise RuntimeError(f"{path} 失败: {body.get('msg') or body}")
        return body.get("data")

    def init(self) -> dict:
        """查询今日晚点名计划与状态（POST，明文，对应前端 createCommonApi）。"""
        url = f"{BASE_URL}{API_PREFIX}/init.action"
        resp = self.http.post(url, json={}, timeout=15)
        resp.raise_for_status()
        body = resp.json()
        if not (body.get("success") and body.get("code") == 1):
            raise RuntimeError(f"init 失败: {body.get('msg') or body}")
        return body.get("data") or {}

    def server_time(self):
        """服务器时间戳（毫秒），用于迟到判定与本地时间校准。"""
        try:
            data = self._enc_get("currentTimestamp.action", {})
            return (data or {}).get("timestamp")
        except Exception:
            return None

    def check(self, longitude: float, latitude: float, school_position) -> list:
        """校验定位是否在签到范围，返回命中的校区/网格列表（取[0]用于打卡参数）。"""
        payload = {
            "mapType": "amap",
            "longitude": longitude,
            "latitude": latitude,
            "range": json.dumps(school_position, separators=(",", ":")),
        }
        data = self._enc_get("check.action", payload)
        return data if isinstance(data, list) else []

    def clock_in(self, *, campus_id, longitude, latitude, start_time, end_time,
                 actual_location, coll_unit, way, is_late) -> dict:
        """提交打卡（GET + 加密 param），返回 {attn: {...}}。"""
        payload = {
            "campus": campus_id or "",
            "lon": longitude,
            "lat": latitude,
            "startTime": start_time,
            "endTime": end_time,
            "actualLocation": actual_location,
            "caIsNo": "0",
            "collUnit": coll_unit,
            "way": way,
            "isLate": 1 if is_late else 0,
        }
        return self._enc_get("clockIn.action", payload) or {}


# ============ 对 main.py 的两个适配入口 ============

def query_today_task(client: AttnClient, cfg: dict) -> dict:
    """查询当天状态。返回 {"need_checkin": bool, "init": init响应}。"""
    data = client.init()
    user = (data.get("userData") or {})
    # FState 取值：未签到 / SUCCESS（已签）/ 已签到（兼容不同版本）
    done_states = {"SUCCESS", "已签到"}
    return {"need_checkin": user.get("FState") not in done_states, "init": data}


def do_checkin(client: AttnClient, cfg: dict, init_data: dict) -> bool:
    """按 init 数据 + 配置定位完成 check → clockIn 全链路。"""
    user = init_data["userData"]
    params = init_data["paramsData"]
    # 基准点可能来自位置池（每次运行换一个点），文案与基准点绑定，统一由
    # resolve_location 解析；抖动半径单独由 get_jitter_radius 决定（0 = 不抖）
    place = resolve_location(cfg)
    base_lon, base_lat = place["longitude"], place["latitude"]
    radius_m = get_jitter_radius(cfg)
    if place["label"] != "single":
        print(f"本次签到基准位置取自位置池 {place['label']}：{place['actual_location'] or '（无文案）'}")

    # 1. 组装签到范围并做定位校验（schoolPosition = {range: [多边形...]}）
    polygons = []
    for sd in init_data.get("schoolData") or []:
        try:
            polygons.extend(json.loads(sd["FPosition"]).get("range") or [])
        except Exception:
            continue

    # 在半径内随机取点，check 与 clockIn 使用同一个点（模拟真实设备一次性定位）。
    # 正常情况只发 1 次 check（与不抖动时一致），不额外增加请求特征。
    longitude, latitude = base_lon, base_lat
    matched = None
    if radius_m > 0:
        for attempt in range(JITTER_MAX_ATTEMPTS):
            j_lon, j_lat = jitter_location(base_lon, base_lat, radius_m)
            j_matched = client.check(j_lon, j_lat, {"range": polygons})
            if j_matched:
                longitude, latitude, matched = j_lon, j_lat, j_matched
                break
            print(f"第 {attempt + 1} 次抖动点落在签到范围外，重新取点…")

    if matched is None:
        # 未开启抖动，或抖动点全部越界（配置点贴近校区边缘）：退回配置坐标校验，
        # 宁可上报固定坐标，也不因为抖动导致漏签。
        if radius_m > 0:
            print("抖动点均在范围外，回退到配置坐标打卡。")
        matched = client.check(base_lon, base_lat, {"range": polygons})
        if not matched:
            return False

    # 2. 迟到判定：以服务器时间为准（换算成北京时间），与 FLateTime（迟到线）比较
    ts = client.server_time()
    now = beijing_now(ts)
    late_line = (params.get("FLateTime") or params.get("FEndTime") or "23:59").strip()
    try:
        h, m = map(int, late_line.split(":")[:2])
        is_late = now.hour * 60 + now.minute > h * 60 + m
    except Exception:
        is_late = False

    # 3. 提交打卡
    client.clock_in(
        campus_id=(matched[0] or {}).get("id") or user.get("FDefaultCampus"),
        longitude=longitude,
        latitude=latitude,
        start_time=(params.get("FStartTime") or "").strip(),
        end_time=(params.get("FEndTime") or "").strip(),
        actual_location=place["actual_location"],
        coll_unit=user.get("FCollUnit"),
        way=user.get("FWay"),
        is_late=is_late,
    )
    return True
