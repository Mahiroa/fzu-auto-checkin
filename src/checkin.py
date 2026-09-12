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

# 打卡坐标随机抖动：默认半径 200 米，配置 jitter_radius_m: 0 可关闭
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
    base_lon = float(cfg["checkin"]["longitude"])
    base_lat = float(cfg["checkin"]["latitude"])
    # 抖动半径：缺省/留空/写错都按默认 200m；显式 0（或负数）表示关闭抖动
    radius_m = cfg["checkin"].get("jitter_radius_m")
    if radius_m is None or (isinstance(radius_m, str) and not radius_m.strip()):
        radius_m = DEFAULT_JITTER_RADIUS_M
    try:
        radius_m = float(radius_m)
    except (TypeError, ValueError):
        radius_m = DEFAULT_JITTER_RADIUS_M

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
        actual_location=cfg["checkin"].get("actual_location", ""),
        coll_unit=user.get("FCollUnit"),
        way=user.get("FWay"),
        is_late=is_late,
    )
    return True
