/* =============================================================================
 * fzu-auto-checkin · 地图选点（懒加载模块：零 key、零付费、可自动降级）
 *
 * 为什么不用高德 JS API 2.0：
 *   1) 它要申请 key + 安全密钥（个人开发者免费额度有上限，用超了要充值）；
 *   2) SDK 会往页面里注入行内 <style>，跟本服务的 CSP（style-src 'self'）直接冲突，
 *      为了一个选点弹窗把整站 CSP 放开 'unsafe-inline' 不划算；
 *   3) 它的 POI 检索是另一套 key。
 * 所以地图本身直接铺高德的**瓦片**（webrd0x 街道 / webst0x 卫星，公开、免 key）：
 * 瓦片本身就是 GCJ-02 的 Web 墨卡托，点出来的经纬度天然就是学校判定用的那套坐标，
 * 一格都不用换算。拖动、缩放、落点全部由本文件实现，不引任何第三方脚本。
 *
 * 网络挡住高德时：连续 4 张瓦片失败就自动换 OpenStreetMap 底图（WGS-84），
 * 选点结果再用 app.js 注入的 toGcj 换算回 GCJ-02 —— 用户拿到的永远是能用的坐标。
 *
 * 「上报地点文字」要跟智汇福大一个格式：校区 + 空格 + 详细地址 + 靠近 POI。
 *     旗山校区 福建省福州市闽侯县学府南路辅路28号靠近福州大学旗山校区生活区X号楼
 * 前半段是门牌级地址、后半段是楼栋/POI，「靠近」是格式的一部分。只有同一处被写了
 * 两遍（高德已经把楼栋写进 formatted_address，又拼一次同名 POI）才合并成一句；
 * 多行粘贴带来的空白也在这里收口（normalizePlace，与 server/internal/model/
 * address.go、src/checkin.py 同一套规则）。
 * 这串里"省市区 + 路名门牌 + 靠近 POI"只有中文逆地理编码能拼出来（OSM 在校园里
 * 只到"闽侯县"这一级，拼不出门牌号），所以地名服务走**可选的**高德 Web 服务 key：
 *   - 填了 key：点一下就让高德 regeo 生成同格式文本，并缓存到这个点位上次的记录里；
 *   - 没填 key：不联网、不瞎编地址，改成"这个点上次用过的文字自动带回来"，
 *     第一次手写一遍，之后同一个位置（±11 米）都自动填。
 * key 只存这台机器的 localStorage，不进配置文件、不随导出走。
 *
 * 只在用户第一次点「地图选点」时被 app.js 动态 import：首屏不加载，不用地图就
 * 永远不会向第三方发一个请求。动态内容一律 createElement + textContent 生成，
 * 不拼 HTML 字符串（与 app.js 同一条约定）。
 * ========================================================================== */

'use strict';

/* ---------- 0. 常量 ---------- */

const TILE = 256;
const ZOOM_MIN = 5;
const ZOOM_MAX = 18;          // 高德瓦片到 18 级；再往上没有图源，放大了只是糊
const MAX_LAT = 85.05112878;  // Web 墨卡托的极点
const HOME = { lng: 119.209433, lat: 26.046112 }; // 福大旗山校区（GCJ-02）
const TILE_ERROR_LIMIT = 4;   // 连续失败多少张瓦片就判定"高德不通"

/* 上报地点文字的规范形态（与 server/internal/model/address.go 同一套规则）：
     旗山校区 福建省福州市闽侯县学府南路辅路28号靠近福州大学旗山校区生活区X号楼
   第一段固定是校区标记；正文是"省市区县 + 道路门牌"，再接"靠近 + POI 全称"。
   「靠近」不是冗余：门牌管路、POI 管楼。只有 POI 已经写进正文（高德 regeo 落到
   楼栋时就是这样）才不会重复一遍——否则就成了"…学生公寓靠近…学生公寓"，同一处
   说两遍（见 normalizePlace）。真的不在旗山校区时，用户在这场文本里直接改掉即可
   （它就是个普通输入框）。 */
const CAMPUS_PREFIX = '旗山校区';
const NEAR_SEP = '靠近';
const ADMIN_HEADS = ['福建省', '福州市', '闽侯县'];
const MERGE_MIN = 4;      // 两段首尾重叠少于 4 字不当作同一段（"学园路" + "路28号" 只重 1 字）
const DUP_MIN = 3;        // 相邻完全重复少于 3 字不折叠（"上街镇" + "镇上" 是正常连读）
const REGEO_URL = 'https://restapi.amap.com/v3/geocode/regeo';
const KEY_STORE = 'fzu.map.regeoKey';       // 只存本机浏览器，不进配置、不随导出走
const NAME_STORE = 'fzu.map.placeNames';    // 点位 → 上次用的上报地点文字
const NAME_STORE_MAX = 200;                 // 一条 ~70 字节，封顶约 14 KB
const NAME_MEMO_DECIMALS = 4;               // 4 位小数 ≈ 11 米，够认定"同一个位置"
const REGEO_DEBOUNCE_MS = 450;

/* 每个图层的三件事：瓦片地址怎么拼、坐标系是哪套、版权声明写什么 */
const LAYERS = {
  road: {
    name: '高德街道', gcj: true, attr: '© 高德地图',
    url: function (x, y, z) {
      const s = (x + y) % 4 + 1;
      return 'https://webrd0' + s + '.is.autonavi.com/appmaptile?lang=zh_cn&size=1&scale=1&style=8&x=' + x + '&y=' + y + '&z=' + z;
    }
  },
  sat: {
    name: '高德卫星', gcj: true, attr: '© 高德地图',
    url: function (x, y, z) {
      const s = (x + y) % 4 + 1;
      return 'https://webst0' + s + '.is.autonavi.com/appmaptile?style=6&x=' + x + '&y=' + y + '&z=' + z;
    }
  },
  osm: {
    name: 'OpenStreetMap', gcj: false, attr: '© OpenStreetMap 贡献者',
    url: function (x, y, z) {
      return 'https://tile.openstreetmap.org/' + z + '/' + x + '/' + y + '.png';
    }
  }
};

/* 全部可变状态都收在这一个对象里：模块级散变量最容易在"关掉再打开"后留下脏值 */
const st = {
  lng: HOME.lng, lat: HOME.lat, zoom: 16,
  layer: 'road',
  picked: null,               // 已选点（坐标系跟随当前图层）
  nameAuto: true,             // 地点文字框里现在是"自动填的"（用户一动手就变 false）
  nearName: '',               // 高德分享链接里带的 POI 名，拼地址时当"靠近…"用
  key: '',                    // 高德 Web 服务 key（可空；空了就不联网取地名）
  regeoSeq: 0,                // 请求序号：晚到的旧响应不许覆盖新选的地址
  regeoBusy: false,
  timer: 0,                   // 取地名的防抖
  notice: '',                 // 状态行上的临时提示（降级/报错），空表示显示自动算的元信息
  shift: null,                // WGS-84 → GCJ-02，由 app.js 注入（浏览器定位用的同一个函数）
  onConfirm: null,
  returnFocus: null,
  tiles: new Map(),           // "z:tx:ty" → <img>
  tileErrors: 0,
  fellBack: false,            // 本次会话是否已经降级过（降级只做一次，避免来回抖）
  drag: null
};

const ui = {};

/* ---------- 1. 墨卡托投影与坐标工具 ---------- */

function clampLat(lat) {
  return Math.max(-MAX_LAT, Math.min(MAX_LAT, Number(lat)));
}

function wrapLng(lng) {
  let v = Number(lng);
  if (!Number.isFinite(v)) return 0;
  v = ((v + 180) % 360 + 360) % 360 - 180;
  return v;
}

/* 经纬度（当前图层的坐标系）→ 世界像素。高德瓦片就是按 GCJ-02 当 WGS-84 投影的，
   所以 GCJ-02 的经纬度直接套这个公式就能对上瓦片，中间没有任何偏移量。 */
function worldPx(lng, lat, z) {
  const scale = TILE * Math.pow(2, z);
  const latR = clampLat(lat) * Math.PI / 180;
  const s = Math.sin(latR);
  return {
    x: (wrapLng(lng) + 180) / 360 * scale,
    y: (0.5 - Math.log((1 + s) / (1 - s)) / (4 * Math.PI)) * scale
  };
}

function pxToLngLat(x, y, z) {
  const scale = TILE * Math.pow(2, z);
  const n = Math.PI - 2 * Math.PI * y / scale;
  return {
    lng: wrapLng(x / scale * 360 - 180),
    lat: 180 / Math.PI * Math.atan(0.5 * (Math.exp(n) - Math.exp(-n)))
  };
}

/* 注入不进来时按"不偏移"处理：宁可差几百米也别让选点按钮直接报错 */
function wgsToGcj(lng, lat) {
  if (typeof st.shift === 'function') {
    const g = st.shift(lng, lat);
    if (g && Number.isFinite(g.lng) && Number.isFinite(g.lat)) return { lng: g.lng, lat: g.lat };
  }
  return { lng: lng, lat: lat };
}

/* GCJ-02 → WGS-84：没有解析逆，用"减掉同点偏移量"迭代两次，误差在 1 米内 */
function gcjToWgs(lng, lat) {
  if (typeof st.shift !== 'function') return { lng: lng, lat: lat };
  let g = { lng: lng, lat: lat };
  for (let i = 0; i < 2; i++) {
    const f = wgsToGcj(g.lng, g.lat);
    g = { lng: g.lng - (f.lng - lng), lat: g.lat - (f.lat - lat) };
  }
  return g;
}

/* 当前选中点 → GCJ-02（学校判定用的那套）；OSM 图层需要在这里补一次换算 */
function pickedGcj() {
  if (!st.picked) return null;
  if (LAYERS[st.layer].gcj) {
    return { lng: st.picked.lng, lat: st.picked.lat, shifted: false };
  }
  const g = wgsToGcj(st.picked.lng, st.picked.lat);
  return { lng: g.lng, lat: g.lat, shifted: true };
}

/* 只认"真的填了数字"的坐标：Number('') === 0，不挡掉空串就会把地图摆到 (0,0) 的几内亚湾，
   用户还可能顺手确认下去 —— 这是空输入最容易踩的坑。 */
function toNum(v) {
  if (v === null || v === undefined) return NaN;
  const s = String(v).trim();
  if (s === '') return NaN;
  return Number(s);
}

function validLatLng(lng, lat) {
  const a = toNum(lng), b = toNum(lat);
  return Number.isFinite(a) && Number.isFinite(b) &&
    Math.abs(a) <= 180 && Math.abs(b) <= 90;
}

function fmtPair(lng, lat) {
  return Number(lng).toFixed(6) + ', ' + Number(lat).toFixed(6);
}

/* ---------- 2. 解析「坐标 / 高德分享链接」 ---------- */

/* 支持的写法（高德 App「分享 → 复制链接」贴进来就能用）：
     119.209433,26.046112
     119.209433 26.046112
     https://uri.amap.com/marker?position=119.209433,26.046112&name=福州大学…
     https://www.amap.com/regeo?lng=119.209433&lat=26.046112
   返回值里的 name 只在高德链接带 name= 时才有（用来顺手填「上报地点文字」）。 */
/* 导出是为了能被 node 直接 import 做纯函数冒烟（不依赖 DOM）；浏览器侧照常用。 */
export function parsePoint(text) {
  const s = String(text || '').trim();
  if (!s) return null;

  const nameM = /[?&]name=([^&\s]+)/i.exec(s);
  let name = '';
  if (nameM) {
    try { name = decodeURIComponent(nameM[1].replace(/\+/g, ' ')); } catch (e) { name = nameM[1]; }
  }

  const pos = /[?&]position=(-?\d{1,3}(?:\.\d+)?)(?:%2C|,)\s*(-?\d{1,2}(?:\.\d+)?)/i.exec(s);
  if (pos) return { lng: Number(pos[1]), lat: Number(pos[2]), name: name };

  const lngM = /[?&](?:lng|lon|longitude|long)=(-?\d{1,3}(?:\.\d+)?)/i.exec(s);
  const latM = /[?&](?:lat|latitude)=(-?\d{1,2}(?:\.\d+)?)/i.exec(s);
  if (lngM && latM) return { lng: Number(lngM[1]), lat: Number(latM[1]), name: name };

  /* 纯数字对：先按「经度,纬度」读；不合法就当用户写反了（纬度,经度）再兜一次 */
  const pair = /(-?\d{1,3}(?:\.\d+)?)\s*[,，;；\s]\s*(-?\d{1,3}(?:\.\d+)?)/.exec(s);
  if (!pair) return null;
  const a = Number(pair[1]), b = Number(pair[2]);
  if (validLatLng(a, b)) return { lng: a, lat: b, name: name };
  if (validLatLng(b, a)) return { lng: b, lat: a, name: name };
  return null;
}

/* ---------- 2.1 上报地点文字：同格式拼装 + 点位记忆 +（可选）高德逆地理编码 ---------- */

/* localStorage 在隐私模式/被禁用时会抛异常。地名功能坏掉不该把选点带崩，所以一律包一层。 */
function loadStore(key, fallback) {
  try {
    const raw = window.localStorage.getItem(key);
    if (!raw) return fallback;
    const v = JSON.parse(raw);
    return (v === null || v === undefined) ? fallback : v;
  } catch (e) { return fallback; }
}

function saveStore(key, value) {
  try { window.localStorage.setItem(key, JSON.stringify(value)); } catch (e) { /* 存不下就算了 */ }
}

/* 规范形态：旗山校区 + 空格 + 正文；正文里的重复段与多余的"靠近"在这里收口。
   与 server/internal/model/address.go、src/checkin.py 三处同一套规则——服务端保存
   时还会再归一一次（幂等），所以这里漏掉的边角不会流到上报报文里。 */
export function normalizePlace(raw) {
  const text = String(raw || '')
    .replace(/[\u200b\ufeff]/g, ' ')
    .split(/\s+/).filter(Boolean).join(' ');
  if (!text) return '';
  const tagged = text.indexOf(CAMPUS_PREFIX) === 0;
  let body = tagged ? text.slice(CAMPUS_PREFIX.length).trim() : text;
  if (body.indexOf(CAMPUS_PREFIX) === 0) body = body.slice(CAMPUS_PREFIX.length).trim();
  body = collapseRepeat(mergeNear(body)).trim();
  if (!body) return tagged ? CAMPUS_PREFIX : '';
  if (tagged || ADMIN_HEADS.some(function (h) { return body.indexOf(h) === 0; })) {
    return CAMPUS_PREFIX + ' ' + body;
  }
  return body;
}

/* 把 "…靠近X" 并进主地址：X 已包含在主地址里就整段丢掉（就是那个重复的楼栋名）。 */
function mergeNear(text) {
  const parts = String(text).split(NEAR_SEP);
  if (parts.length < 2) return text;
  let merged = parts[0].trim();
  for (let i = 1; i < parts.length; i++) merged = mergeHeadTail(merged, parts[i].trim());
  return merged;
}

function mergeHeadTail(head, tail) {
  if (!tail) return head;
  if (!head) return tail;
  if (head.indexOf(tail) >= 0) return head;   // 主地址已经写到这栋楼
  if (tail.indexOf(head) >= 0) return tail;   // 反过来被 POI 名包含：用更完整的那段
  const limit = Math.min(head.length, tail.length);
  for (let k = limit; k >= MERGE_MIN; k--) {
    if (head.slice(head.length - k) === tail.slice(0, k)) return head + tail.slice(k);
  }
  return head + NEAR_SEP + tail;              // 真的不是同一处：把连接词补回去
}

/* 相邻两段逐字相同（"XX"）只留一段，循环到稳定为止。 */
function collapseRepeat(text) {
  let s = String(text);
  for (;;) {
    let cut = -1;
    let len = 0;
    for (let i = 0; i < s.length && cut < 0; i++) {
      for (let k = Math.floor((s.length - i) / 2); k >= DUP_MIN; k--) {
        if (s.slice(i, i + k) === s.slice(i + k, i + 2 * k)) { cut = i; len = k; break; }
      }
    }
    if (cut < 0) return s;
    s = s.slice(0, cut + len) + s.slice(cut + 2 * len);
  }
}

/* 智汇福大的格式：校区标记 + 空格 + 详细地址（+ 靠近POI，且只在真的说到别处时保留）。
   缺哪段就不硬凑——只有 POI 时是"旗山校区 福州大学旗山校区生活区X号楼"。 */
function composePlace(addr, near) {
  const a = String(addr || '').trim();
  const n = String(near || '').trim();
  if (!a && !n) return '';
  return normalizePlace(a + (n ? NEAR_SEP + n : ''));
}

function memoKey(lng, lat) {
  return Number(lng).toFixed(NAME_MEMO_DECIMALS) + ',' + Number(lat).toFixed(NAME_MEMO_DECIMALS);
}

function lookupName(lng, lat) {
  const all = loadStore(NAME_STORE, {});
  const hit = all[memoKey(lng, lat)];
  return hit && typeof hit.v === 'string' ? hit.v : '';
}

/* 记住"这个点位上次用的文字"：没 key 的用户第一次手写一遍，之后同一位置自动带出。
   按时间淘汰，封顶 NAME_STORE_MAX 条，别把 localStorage 塞满。 */
function rememberName(lng, lat, text) {
  const t = String(text || '').trim();
  if (!t) return;
  const all = loadStore(NAME_STORE, {});
  all[memoKey(lng, lat)] = { v: t, t: Date.now() };
  const keys = Object.keys(all);
  if (keys.length > NAME_STORE_MAX) {
    keys.sort(function (x, y) { return (all[x].t || 0) - (all[y].t || 0); });
    keys.slice(0, keys.length - NAME_STORE_MAX).forEach(function (k) { delete all[k]; });
  }
  saveStore(NAME_STORE, all);
}

/* 高德 regeo：location 必须是 "经度,纬度"；extensions=all 才有 pois/aois 用来拼"靠近…" */
async function fetchPlace(lng, lat) {
  const url = REGEO_URL + '?key=' + encodeURIComponent(st.key) +
    '&location=' + Number(lng).toFixed(6) + ',' + Number(lat).toFixed(6) +
    '&extensions=all&radius=150&roadlevel=0';
  const resp = await fetch(url, { credentials: 'omit', referrerPolicy: 'no-referrer' });
  if (!resp.ok) throw new Error('HTTP ' + resp.status);
  const data = await resp.json();
  if (!data || String(data.status) !== '1') {
    throw new Error(String((data && (data.info || data.infocode)) || '高德返回失败'));
  }
  const rg = data.regeocode || {};
  const pois = Array.isArray(rg.pois) ? rg.pois : [];
  const aois = Array.isArray(rg.aois) ? rg.aois : [];
  const near = st.nearName ||
    String((pois[0] && pois[0].name) || (aois[0] && aois[0].name) || '');
  return composePlace(rg.formatted_address, near);
}

/* 只有"自动生成的文字"允许被下一次自动结果覆盖；用户一动手（nameAuto=false）就不许抢，
   除非是他自己点了「取地名」这种明确要求覆盖的动作（force）。 */
function applyName(text, force) {
  if (!ui.name) return false;
  const v = normalizePlace(text);
  if (!v) return false;
  if (!force && !st.nameAuto) return false;
  ui.name.value = v;
  st.nameAuto = true;
  return true;
}

/* 落点变了就重算一次地名，优先级：
   1) 粘进来的高德分享链接带的 POI（用户明确指定）——有 key 时只借用它的名字当"靠近…"；
   2) 这个点位上次确认过的文字（不联网、不花配额）；
   3) 配了 key → 高德 regeo 生成同格式文本；
   4) 都没有 → 保留用户手写的，绝不凭空编地址。 */
/* 本地能立刻定的部分（记忆/粘贴链接）先同步填上：这些不联网、零延迟，
   不能等 450 ms 的防抖 —— 用户"落点后马上点确认"是最常见的操作顺序。 */
function refreshPlaceLocal() {
  const g = pickedGcj();
  if (!g || !ui.name) return false;
  if (st.nearName && !st.key) return applyName(composePlace('', st.nearName), true);
  const memo = lookupName(g.lng, g.lat);
  return memo ? applyName(memo, false) : false;
}

function schedulePlace() {
  if (st.timer) { clearTimeout(st.timer); st.timer = 0; }
  st.timer = setTimeout(function () { st.timer = 0; refreshPlace(false); }, REGEO_DEBOUNCE_MS);
}

function refreshPlace(force) {
  if (st.timer) { clearTimeout(st.timer); st.timer = 0; }
  const g = pickedGcj();
  if (!g || !ui.name) return;
  if (!force && !st.nameAuto) return;          // 用户在写自己的文案，不去动它、也不浪费配额
  if (st.nearName && !st.key) { applyName(composePlace('', st.nearName), true); return; }
  /* 记忆优先是为了省额度；但用户点名要「取地名」时必须真的去问一次，不能被记忆挡住 */
  if (!force) {
    const memo = lookupName(g.lng, g.lat);
    if (memo) { applyName(memo, false); return; }
  }
  if (!st.key) return;
  fetchPlaceInto(g, force);
}

async function fetchPlaceInto(g, force) {
  const seq = ++st.regeoSeq;
  st.regeoBusy = true;
  if (ui.regeo) ui.regeo.disabled = true;
  try {
    const text = await fetchPlace(g.lng, g.lat);
    if (seq !== st.regeoSeq) return;            // 期间的落点已经变了，丢掉这次结果
    if (!text) {
      setStatus('高德没返回可用的地址，手写一个吧（写过的点会自动记住）。', 'warn');
      return;
    }
    applyName(text, true);
    setStatus('已按高德逆地理编码生成上报地点文字，可自行修改后确认。', 'ok');
  } catch (e) {
    if (seq !== st.regeoSeq) return;
    setStatus('取地名失败：' + (e && e.message ? e.message : '未知错误') +
      '。坐标不受影响，地名可以手写（写过的点会自动记住）。', 'err');
  } finally {
    if (seq === st.regeoSeq) {
      st.regeoBusy = false;
      if (ui.regeo) ui.regeo.disabled = false;
    }
  }
}

/* key 只活在浏览器 localStorage：不进 config.yaml、不随导出走，换台机器就要重填。 */
function saveKey() {
  st.key = ui.key ? String(ui.key.value || '').trim() : '';
  saveStore(KEY_STORE, st.key);
  if (ui.key) ui.key.value = st.key;
  if (!st.key) {
    setStatus('已清空 key：之后不再联网取地名，改用「这个点上次用过的文字」自动带出。', 'ok');
    return;
  }
  setStatus('key 已存在这台电脑的浏览器里（不写配置文件、不随导出）。正在取一次地名…', 'ok');
  refreshPlace(true);
}

/* ---------- 3. 地图引擎：瓦片、拖动、缩放、落点 ---------- */

function centerWorld() {
  return worldPx(st.lng, st.lat, st.zoom);
}

function tileUrl(tx, ty, z) {
  const n = Math.pow(2, z);
  const x = ((tx % n) + n) % n;      // 横向环绕：跨 180° 经线时左右接得上
  return LAYERS[st.layer].url(x, ty, z);
}

function makeTile(tx, ty) {
  const img = document.createElement('img');
  img.alt = '';
  img.draggable = false;
  img.decoding = 'async';
  img.addEventListener('error', onTileError);
  img.src = tileUrl(tx, ty, st.zoom);
  return img;
}

/* 瓦片挂了不代表整个功能要跟着挂：先记数，连续失败够多就换 OSM 底图 */
function onTileError() {
  st.tileErrors += 1;
  if (st.layer === 'osm' || st.fellBack || st.tileErrors < TILE_ERROR_LIMIT) return;
  st.fellBack = true;
  setLayer('osm', true);
  setStatus('高德瓦片加载不出来（多半是网络受限），已临时换成 OpenStreetMap 底图；' +
    '选点结果会自动换算成高德坐标（GCJ-02），照常能用。', 'warn');
}

function clearTiles() {
  st.tiles.forEach(function (img) { img.remove(); });
  st.tiles.clear();
}

/* 铺瓦片：按当前中心算出需要哪几块，缺的补、多的删，已有的只挪位置。
   不整体重建 DOM —— 拖动时每帧都在 render，重建会让图一直闪。 */
function render() {
  if (!ui.map) return;
  const W = ui.map.clientWidth;
  const H = ui.map.clientHeight;
  if (!W || !H) return;

  const c = centerWorld();
  const left = c.x - W / 2;
  const top = c.y - H / 2;
  const n = Math.pow(2, st.zoom);
  const x0 = Math.floor(left / TILE);
  const x1 = Math.floor((left + W - 1) / TILE);
  const y0 = Math.floor(top / TILE);
  const y1 = Math.floor((top + H - 1) / TILE);
  const keep = new Set();

  for (let ty = y0; ty <= y1; ty++) {
    if (ty < 0 || ty >= n) continue; // 纵向不环绕：超出极点的行本来就没图
    for (let tx = x0; tx <= x1; tx++) {
      const key = st.zoom + ':' + tx + ':' + ty;
      keep.add(key);
      let img = st.tiles.get(key);
      if (!img) {
        img = makeTile(tx, ty);
        st.tiles.set(key, img);
        ui.tiles.append(img);
      }
      img.style.transform = 'translate(' + Math.round(tx * TILE - left) + 'px,' +
        Math.round(ty * TILE - top) + 'px)';
    }
  }
  st.tiles.forEach(function (img, key) {
    if (keep.has(key)) return;
    img.remove();
    st.tiles.delete(key);
  });

  renderPin(left, top);
  renderMeta();
}

function renderPin(left, top) {
  if (!ui.pin) return;
  if (!st.picked) { ui.pin.hidden = true; return; }
  const p = worldPx(st.picked.lng, st.picked.lat, st.zoom);
  ui.pin.hidden = false;
  ui.pin.style.left = Math.round(p.x - left) + 'px';
  ui.pin.style.top = Math.round(p.y - top) + 'px';
}

function metersPerPixel() {
  return 156543.03392 * Math.cos(clampLat(st.lat) * Math.PI / 180) / Math.pow(2, st.zoom);
}

function renderMeta() {
  if (ui.attr) ui.attr.textContent = LAYERS[st.layer].attr;
  const g = pickedGcj();
  const parts = [
    '中心 ' + fmtPair(st.lng, st.lat) + ' · ' + st.zoom + ' 级 · 约 ' +
    metersPerPixel().toFixed(1) + ' 米/像素'
  ];
  if (g) {
    parts.push('已选 ' + fmtPair(g.lng, g.lat) + '（GCJ-02' + (g.shifted ? '，已从 WGS-84 换算' : '') + '）');
  } else {
    parts.push('还没选点：在地图上点一下，或点「用中心」');
  }
  if (ui.status && !st.notice) ui.status.textContent = parts.join(' ｜ ');
  if (ui.open) {
    const p = g || { lng: st.lng, lat: st.lat };
    ui.open.href = 'https://uri.amap.com/marker?position=' +
      Number(p.lng).toFixed(6) + ',' + Number(p.lat).toFixed(6) + '&coordinate=gaode&callnative=0';
  }
}

/* 状态行有两类内容：render 自动算的元信息、以及要留一会儿的提示（降级/报错）。
   后者一写进去就把元信息顶掉，直到下一次用户操作再恢复。 */
function setStatus(text, kind) {
  if (!ui.status) return;
  st.notice = text === '' ? '' : (text || '');
  ui.status.textContent = st.notice || '';
  ui.status.classList.remove('ok', 'warn', 'err');
  if (kind) ui.status.classList.add(kind);
  if (!st.notice) renderMeta();
}

function setPickedAt(lng, lat) {
  st.picked = { lng: wrapLng(lng), lat: clampLat(lat) };
  st.nearName = '';   // 手动挪点以后，粘链接带进来的那个 POI 就不再对应这个位置了
  if (st.notice) { st.notice = ''; if (ui.status) ui.status.classList.remove('warn', 'err', 'ok'); }
  render();
  /* 地名跟着落点走：本地能定的立刻填，剩下的才排一次防抖（避免拖点抖出一串请求） */
  if (!refreshPlaceLocal()) schedulePlace();
}

function setCenter(lng, lat, zoom) {
  st.lng = wrapLng(lng);
  st.lat = clampLat(lat);
  if (zoom) st.zoom = Math.max(ZOOM_MIN, Math.min(ZOOM_MAX, Math.round(zoom)));
  render();
}

/* 以视口内某个像素为锚点缩放：锚点下那块地皮不动，符合"滚轮往哪儿滚就往哪儿钻" */
function zoomAt(px, py, delta) {
  const z2 = Math.max(ZOOM_MIN, Math.min(ZOOM_MAX, st.zoom + delta));
  if (z2 === st.zoom) return;
  const W = ui.map.clientWidth, H = ui.map.clientHeight;
  const c = centerWorld();
  const anchor = pxToLngLat(c.x - W / 2 + px, c.y - H / 2 + py, st.zoom);
  const aw = worldPx(anchor.lng, anchor.lat, z2);
  st.zoom = z2;
  const nc = { x: aw.x - (px - W / 2), y: aw.y - (py - H / 2) };
  const p = pxToLngLat(nc.x, nc.y, z2);
  st.lng = p.lng;
  st.lat = p.lat;
  render();
}

function zoomCenter(delta) {
  const W = ui.map ? ui.map.clientWidth : 0, H = ui.map ? ui.map.clientHeight : 0;
  zoomAt(W / 2, H / 2, delta);
}

/* 指针交互：拖动平移；没拖动的"点一下"= 在这里落点 */
function localPoint(ev) {
  const rect = ui.map.getBoundingClientRect();
  return { x: ev.clientX - rect.left, y: ev.clientY - rect.top };
}

function onPointerDown(ev) {
  if (ev.button !== undefined && ev.button !== 0) return;
  ev.preventDefault();
  st.drag = { id: ev.pointerId, x: ev.clientX, y: ev.clientY, lng: st.lng, lat: st.lat, moved: 0 };
  try { ui.map.setPointerCapture(ev.pointerId); } catch (e) { /* 某些浏览器在没有捕获权时会抛，忽略即可 */ }
  ui.map.classList.add('dragging');
}

function onPointerMove(ev) {
  if (!st.drag || ev.pointerId !== st.drag.id) return;
  const dx = ev.clientX - st.drag.x;
  const dy = ev.clientY - st.drag.y;
  st.drag.moved = Math.max(st.drag.moved, Math.abs(dx) + Math.abs(dy));
  const c = worldPx(st.drag.lng, st.drag.lat, st.zoom);
  const p = pxToLngLat(c.x - dx, c.y - dy, st.zoom);
  st.lng = p.lng;
  st.lat = p.lat;
  render();
}

function onPointerUp(ev) {
  if (!st.drag || ev.pointerId !== st.drag.id) return;
  const moved = st.drag.moved;
  st.drag = null;
  ui.map.classList.remove('dragging');
  if (moved >= 5) return; // 拖动过就不算点击，免得手抖把点挪走
  const pt = localPoint(ev);
  const c = centerWorld();
  const p = pxToLngLat(c.x - ui.map.clientWidth / 2 + pt.x, c.y - ui.map.clientHeight / 2 + pt.y, st.zoom);
  setPickedAt(p.lng, p.lat);
}

function onWheel(ev) {
  ev.preventDefault();
  const pt = localPoint(ev);
  zoomAt(pt.x, pt.y, ev.deltaY > 0 ? -1 : 1);
}

function onMapKey(ev) {
  const step = 60;
  const key = ev.key;
  if (key === 'ArrowLeft') { moveCenter(-step, 0); }
  else if (key === 'ArrowRight') { moveCenter(step, 0); }
  else if (key === 'ArrowUp') { moveCenter(0, -step); }
  else if (key === 'ArrowDown') { moveCenter(0, step); }
  else if (key === '+' || key === '=') { zoomCenter(1); }
  else if (key === '-' || key === '_') { zoomCenter(-1); }
  else return;
  ev.preventDefault();
}

function moveCenter(dx, dy) {
  const c = centerWorld();
  const p = pxToLngLat(c.x + dx, c.y + dy, st.zoom);
  st.lng = p.lng;
  st.lat = p.lat;
  render();
}

/* 换图层：坐标系不同的两套图之间必须同时换算中心和已选点，否则会平移几百米 */
function setLayer(name, keepNotice) {
  if (!LAYERS[name]) return;
  if (name === st.layer) return;
  const from = LAYERS[st.layer], to = LAYERS[name];
  if (from.gcj !== to.gcj) {
    const conv = to.gcj ? wgsToGcj : gcjToWgs;
    const c = conv(st.lng, st.lat);
    st.lng = c.lng;
    st.lat = c.lat;
    if (st.picked) {
      const p = conv(st.picked.lng, st.picked.lat);
      st.picked = { lng: p.lng, lat: p.lat };
    }
  }
  st.layer = name;
  st.tileErrors = 0;
  clearTiles();
  if (ui.layerRoad) ui.layerRoad.setAttribute('aria-pressed', name === 'road' ? 'true' : 'false');
  if (ui.layerSat) ui.layerSat.setAttribute('aria-pressed', name === 'sat' ? 'true' : 'false');
  if (!keepNotice) { st.notice = ''; if (ui.status) ui.status.classList.remove('ok', 'warn', 'err'); }
  render();
}

/* ---------- 4. 弹窗 ---------- */

function bind() {
  if (ui.modal) return;
  const g = function (id) { return document.getElementById(id); };
  ui.modal = g('map-picker');
  if (!ui.modal) return;
  ui.close = g('mp-close');
  ui.map = g('mp-map');
  ui.tiles = g('mp-tiles');
  ui.pin = g('mp-pin');
  ui.status = g('mp-status');
  ui.attr = g('mp-attr');
  ui.center = g('mp-center');
  ui.zoomIn = g('mp-zoom-in');
  ui.zoomOut = g('mp-zoom-out');
  ui.layerRoad = g('mp-layer-road');
  ui.layerSat = g('mp-layer-sat');
  ui.open = g('mp-open');
  ui.paste = g('mp-paste');
  ui.parse = g('mp-parse');
  ui.name = g('mp-name');
  ui.regeo = g('mp-regeo');
  ui.keyBox = g('mp-key-box');
  ui.key = g('mp-key');
  ui.keySave = g('mp-key-save');
  ui.ok = g('mp-ok');
  ui.cancel = g('mp-cancel');

  if (ui.close) ui.close.addEventListener('click', function () { close(false); });
  if (ui.cancel) ui.cancel.addEventListener('click', function () { close(false); });
  if (ui.ok) ui.ok.addEventListener('click', confirm);
  if (ui.modal) {
    ui.modal.addEventListener('click', function (ev) { if (ev.target === ui.modal) close(false); });
  }
  if (ui.zoomIn) ui.zoomIn.addEventListener('click', function () { zoomCenter(1); });
  if (ui.zoomOut) ui.zoomOut.addEventListener('click', function () { zoomCenter(-1); });
  if (ui.layerRoad) ui.layerRoad.addEventListener('click', function () { setLayer('road'); });
  if (ui.layerSat) ui.layerSat.addEventListener('click', function () { setLayer('sat'); });
  if (ui.center) ui.center.addEventListener('click', function () {
    setPickedAt(st.lng, st.lat);
    setStatus('已把地图中心设为选点，按住方向键或拖动可以微调。', 'ok');
  });
  if (ui.parse) ui.parse.addEventListener('click', applyPaste);
  if (ui.paste) {
    ui.paste.addEventListener('keydown', function (ev) {
      if (ev.key === 'Enter') { ev.preventDefault(); applyPaste(); }
    });
  }
  /* 地点文字：用户一动笔就锁定（不再被自动结果覆盖），点「取地名」才强制重算 */
  if (ui.name) {
    ui.name.addEventListener('input', function () { st.nameAuto = false; });
    ui.name.addEventListener('keydown', function (ev) { if (ev.key === 'Enter') ev.preventDefault(); });
  }
  if (ui.regeo) {
    ui.regeo.addEventListener('click', function () {
      if (!st.key) {
        setStatus('要自动生成地名，先在下面「地名服务（可选）」里填一个高德 Web 服务 key（免费额度，不用充值）。' +
          '不填也能用：手写一次，这个位置以后会自动带出。', 'warn');
        if (ui.keyBox) ui.keyBox.open = true;
        if (ui.key) ui.key.focus();
        return;
      }
      refreshPlace(true);
    });
  }
  if (ui.keySave) ui.keySave.addEventListener('click', saveKey);
  if (ui.map) {
    ui.map.addEventListener('pointerdown', onPointerDown);
    ui.map.addEventListener('pointermove', onPointerMove);
    ui.map.addEventListener('pointerup', onPointerUp);
    ui.map.addEventListener('pointercancel', onPointerUp);
    ui.map.addEventListener('wheel', onWheel, { passive: false });
    ui.map.addEventListener('keydown', onMapKey);
  }
  document.addEventListener('keydown', function (ev) {
    if (ui.modal.hidden || ev.key !== 'Escape') return;
    ev.stopPropagation();
    close(false);
  }, true);
  window.addEventListener('resize', function () { if (!ui.modal.hidden) render(); });
}

function applyPaste() {
  const text = ui.paste ? String(ui.paste.value || '').trim() : '';
  const p = parsePoint(text);
  if (!p) {
    setStatus('这段文字里没找到坐标。能认的写法：119.209433,26.046112、' +
      'position=119.209433,26.046112，或高德 App「分享 → 复制链接」里的地址。', 'err');
    return;
  }
  const tgt = LAYERS[st.layer].gcj ? p : gcjToWgs(p.lng, p.lat);
  setCenter(tgt.lng, tgt.lat, 17);
  setPickedAt(tgt.lng, tgt.lat);
  /* 状态行报的是"最终要交给服务端的坐标"（GCJ-02）；OSM 图层下顺手把换算说清楚 */
  const g = pickedGcj() || tgt;
  setStatus('已定位到 ' + fmtPair(g.lng, g.lat) + (p.name ? '（' + p.name + '）' : '') +
    '；确认无误后点「用这个点」。', 'ok');
  /* 链接里的 POI 名是用户明确给的：有 key 时把它当"靠近…"，没 key 时它就是全部依据 */
  st.nearName = p.name || '';
  if (st.nearName) refreshPlace(true);
  if (ui.paste) ui.paste.value = '';
}

function confirm() {
  const g = pickedGcj();
  if (!g) {
    setStatus('还没选点：在地图上点一下，或先点「用中心」。', 'err');
    if (ui.map) ui.map.focus();
    return;
  }
  const cb = st.onConfirm;
  /* 用户自己改过的文字也要过一遍归一：他可能把楼栋又写了两遍，或者手抄进来一段
     带换行的地址；确认下来的这串会被记住并原样提交给学校。 */
  let name = normalizePlace(ui.name ? ui.name.value : '');
  /* 防抖还没跑完就点了确认：先把本地记忆同步补上；只有"正在联网取地名"才让他等一下，
     因为这时候放过去就等于静默丢掉了地点文字。 */
  if (!name) {
    const memo = lookupName(g.lng, g.lat);
    if (memo) { name = normalizePlace(memo); applyName(name, true); }
  }
  if (!name && st.regeoBusy) {
    setStatus('正在取地名…稍等一下再点「用这个点」。', 'warn');
    return;
  }
  /* 确认过的文字就是这个点位的"标准答案"：下次落到同一个位置直接带出来，不用再取一次 */
  if (name) rememberName(g.lng, g.lat, name);
  const payload = { lng: g.lng, lat: g.lat, name: name };
  close(true);
  if (typeof cb === 'function') cb(payload);
}

function close(confirmed) {
  if (!ui.modal) return;
  ui.modal.hidden = true;
  st.drag = null;
  if (st.timer) { clearTimeout(st.timer); st.timer = 0; }
  st.regeoSeq += 1;            // 让还在飞的 regeo 响应失效：关掉的弹窗不该再改任何东西
  if (ui.map) ui.map.classList.remove('dragging');
  if (!confirmed) st.notice = '';
  const el = st.returnFocus;
  st.returnFocus = null;
  if (el && typeof el.focus === 'function') el.focus();
}

/* openPicker 的入参：
     lng / lat  打开时把地图摆到这个点（GCJ-02，字符串也认；空则用上次的点或校区默认）
     name       表单里现有的「上报地点文字」，带进来当初始值（空则尝试记忆/取地名）
     toGcj      WGS-84 → GCJ-02 的换算函数（app.js 传 wgs84ToGcj02 进来，避免同一套公式写两遍）
     onConfirm  确认回调，参数 {lng, lat, name}；lng/lat 已保证是 GCJ-02 的 6 位小数 */
export function openPicker(opts) {
  const o = opts || {};
  bind();
  if (!ui.modal) throw new Error('页面缺少地图选点弹窗（index.html 的 #map-picker）');

  st.shift = typeof o.toGcj === 'function' ? o.toGcj : st.shift;
  st.onConfirm = typeof o.onConfirm === 'function' ? o.onConfirm : null;
  st.notice = '';
  st.nearName = '';
  st.returnFocus = document.activeElement;
  st.key = String(loadStore(KEY_STORE, '') || '');
  if (ui.key) ui.key.value = st.key;
  /* 表单里已有的文字先摆进来；它属于"自动那一路"，落点变了可以被新结果替换 */
  const preset = String(o.name || '').trim();
  if (ui.name) ui.name.value = preset;
  st.nameAuto = true;
  st.regeoSeq += 1;            // 上一轮残留的响应一律作废
  if (ui.regeo) ui.regeo.disabled = false;

  const hasCoord = validLatLng(o.lng, o.lat);
  if (hasCoord) {
    const lng = toNum(o.lng), lat = toNum(o.lat);
    /* 传进来的是 GCJ-02；当前若停在 OSM 图层，摆位置前先换算到 WGS-84 */
    const c = LAYERS[st.layer].gcj ? { lng: lng, lat: lat } : gcjToWgs(lng, lat);
    st.lng = c.lng;
    st.lat = c.lat;
    st.zoom = 17;
    st.picked = { lng: c.lng, lat: c.lat };
  } else {
    /* 没坐标：保留上一次的地图视野（第一次打开时它就是校区默认中心），
       只把缩放托到能看清楼的水平 —— 每次重开都跳回原点会让人白找一遍。
       但图钉要清掉：这个槽位本来就没有点，"打开就能确认上次那个点"等于给错坐标。 */
    st.zoom = Math.max(st.zoom, 16);
    st.picked = null;
  }

  ui.modal.hidden = false;
  if (ui.status) ui.status.classList.remove('ok', 'warn', 'err');
  render();
  /* 弹窗刚显示时 clientWidth 可能还是 0（display 刚切换），下一帧再铺一次最稳 */
  requestAnimationFrame(render);
  /* 一打开就把地名补齐：有记忆直接用，有 key 就取一次，都没有就保留表单里的原文 */
  if (st.picked) refreshPlace(false);
  /* 焦点给地图而不是「用这个点」：方向键能直接微调，也不会一按回车就确认 */
  if (ui.map) ui.map.focus();
  return true;
}
