/* =============================================================================
 * fzu-auto-checkin · 管理前端（零构建 / 零依赖 / 原生 ES module）
 *
 * 数据流总览：status → 视图 → 操作 → 重新拉取
 *   1) 启动   GET /auth/session 决定三态之一：设置密码 / 登录 / 进入应用。
 *               未初始化强制「设置管理密码」；未登录只显示登录卡片。
 *   2) 仪表盘 #/dashboard：每 15 s 拉 GET /status + GET /runs?limit=5；
 *              倒计时用 next_run_at 在本地每秒递减（不额外轮询接口）。
 *              按钮 → POST /runs {mode} / POST /notify/test → 重新拉 status+runs。
 *   3) 配置   #/config：GET /config（明文回显）→ fillConfig 填表 → PUT /config {config}；
 *              导入 POST /config/import-yaml 预览（同样明文）→ PUT 应用；
 *              导出 GET /config/export?format=yaml（脱敏版）附件下载。
 *   4) 日志   #/logs：先 GET /logs?limit=200 快照，再 EventSource /logs/stream
 *              （event: log / event: run）增量追加；仅 DOM 局部追加，不整页重渲染。
 *   5) 历史   #/runs：GET /runs?limit=N（limit 分页），点开 GET /runs/{id} 渲染抽屉。
 *
 * 请求约定：所有非 GET 请求由 apiFetch 统一补齐 X-Requested-With: fzu-web
 * 与 credentials:'same-origin'；401 回登录、403 提示 CSRF、429 限速倒计时、5xx toast。
 * 安全约定：动态内容一律 textContent（不拼 innerHTML），避免把上游数据当 HTML 执行。
 * ========================================================================== */

'use strict';

/* ---------- 0. 常量 ---------- */

const API = '/api/v1';
const ROUTES = ['#/dashboard', '#/config', '#/logs', '#/runs'];
const STATUS_EVERY_MS = 15000;
const LOG_INITIAL = 200;
const LOG_CAP = 500;
const LEVELS = { debug: 0, info: 1, warn: 2, error: 3 };

/* MASK_SENTINEL 必须与服务端 config.MaskSentinel 逐字节一致。
 * 语义（两边共用一份协议）：GET /config 把敏感字段换成它；前端原样回传 = "保持原值"，
 * 回传空串 = "清空这一项"。因此这里既不能改字符，也不能在回传前 trim 成空。 */
const MASK_SENTINEL = '••••';

const STATUS_TEXT = {
  success: '签到成功', already_checked: '今日已签', no_plan: '无考勤计划',
  out_of_range: '位置越界', vacation: '假期跳过', skipped: '已跳过', error: '失败'
};
const STATUS_KIND = {
  success: 'ok', already_checked: 'ok', no_plan: 'warn',
  out_of_range: 'err', vacation: 'warn', skipped: 'warn', error: 'err'
};
const SLOT_TEXT = { pending: '待触发', done: '已完成', missed: '已错过' };
const DECISION_TEXT = {
  on_target: '准点执行', wait_until_target: '等待目标时刻', wait_too_long: '等不起，跳过',
  late_fallback: '补签兜底', no_slot_fallback: '无有效档，兜底执行', out_of_window: '窗口外跳过'
};
const NOTIFY_HINT = {
  serverchan: 'Server酱 Turbo：填以 SCT 开头的 SendKey，能收到一条测试消息就是配通了。',
  pushplus: 'PushPlus：填你的 token，能收到一条测试消息就是配通了。',
  bark: 'Bark：填完整的推送地址（里面含你的设备标识）。',
  wecom: '企业微信群机器人：填群机器人的完整地址，能在群里看到测试消息就是配通了。',
  none: '不推送：签到结果只留在「历史」和「日志」里。'
};
const NOTIFY_NAME = {
  serverchan: 'Server酱', pushplus: 'PushPlus', bark: 'Bark',
  wecom: '企业微信群机器人', none: '不推送'
};
const MODE_TEXT = { normal: '自动执行', force: '手动「立即签到」' };
const CONN_TEXT = {
  idle: '未连接', connecting: '连接中…', open: '已连接',
  reconnecting: '重连中…', closed: '已断开（重连中…）', unsupported: '浏览器不支持 SSE'
};

/* ---------- 1. 运行期状态（单一数据源，视图只读它） ---------- */

const S = {
  session: { initialized: false, authed: false, env: '' },
  route: '',
  /* 仪表盘 */
  status: null, skew: 0, runs5: [], slotsSig: '', runsSig: '', zeroGuard: 0,
  statusTimer: 0, tickTimer: 0,
  autoBusy: false, // 请求在飞时，轮询不许覆盖乐观状态
  /* 配置 */
  config: null, configVersion: 0, configLoaded: false, importPreview: null, maskedFields: {},
  /* Cookie 的填写栏已经去掉：老配置里那份默认保留，点过「清掉它」才置位 */
  dropCookie: false,
  pendingReveal: null, // 正在等管理密码的那一项凭据（见 openReveal）
  /* 日志 */
  logs: [], logKeys: new Set(), logsSnapshotted: false, autoScroll: true,
  es: null, esRetry: 0, esTimer: 0,
  /* 历史 */
  runs: [], runsPageSize: 20, runsPages: 1, runsExhausted: false,
  /* 限速 */
  rateUntil: 0, rateTimer: 0
};

/* ---------- 2. 基础设施：选择器 / 格式化 / toast / 错误 ---------- */

const $ = (id) => document.getElementById(id);

function str(v) { return (v === null || v === undefined) ? '' : String(v); }
function numStr(v) { return (v === null || v === undefined || v === '') ? '' : String(v); }

function setVal(id, v) { const el = $(id); if (el) el.value = str(v); }
function getVal(id) { const el = $(id); return el ? String(el.value).trim() : ''; }
function setText(id, t) { const el = $(id); if (el) el.textContent = str(t); }
function setHidden(id, hidden) { const el = $(id); if (el) el.hidden = !!hidden; }
function isHidden(id) { const el = $(id); if (!el) return true; return !!el.hidden; }

/* 打开一层覆盖层，并把它抬到最上面。
 *
 * 四个面层（drawer / 导入 / 显示凭据 / 登录）共用 z-index 40：同一层级下"谁在 DOM
 * 后面谁在上面"。所以每次**真正打开**时把节点挪到 body 末尾，后打开的永远压住先打开
 * 的——弹窗里点眼睛打开的「显示凭据」必须落在弹窗之上，这是实测出来的次序错误。
 * 已经开着就不再挪：否则 showSecondAuth 再叫一次 openLoginModal，会把登录弹窗又抬到
 * 正在上面操作的「显示凭据」之上。
 * 用 CSSOM 写 z-index 是被门禁禁止的（这段代码里出现 style 属性赋值就会红），挪 DOM
 * 是同一效果的干净做法。 */
function showOverlay(id) {
  const el = $(id);
  if (!el) return null;
  const wasHidden = !!el.hidden;
  el.hidden = false;
  if (wasHidden) document.body.append(el);
  return el;
}

function pad2(n) { return n < 10 ? '0' + n : '' + n; }

function hhmmss(d) { return pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds()); }

function toMs(v) {
  if (typeof v === 'number') return v;
  const t = Date.parse(str(v));
  return Number.isFinite(t) && t > 0 ? t : NaN;
}

function fmtTime(v) {
  const t = toMs(v);
  if (!Number.isFinite(t)) return '--:--:--';
  return hhmmss(new Date(t));
}

function fmtDateTime(v) {
  const t = toMs(v);
  if (!Number.isFinite(t)) return '—';
  const d = new Date(t);
  return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + ' ' + hhmmss(d);
}

function fmtLatency(ms) {
  const n = Number(ms);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1000) return n + ' ms';
  if (n < 60000) return (n / 1000).toFixed(2) + ' s';
  return Math.floor(n / 60000) + 'm' + pad2(Math.round((n % 60000) / 1000)) + 's';
}

function fmtNum(v) {
  const n = Number(v);
  return Number.isFinite(n) ? n.toFixed(5) : '—';
}

function numOrNull(v) {
  const s = str(v).trim();
  if (!s) return null;
  const n = Number(s);
  return Number.isFinite(n) ? n : null;
}

function parseJSON(text) {
  try { return JSON.parse(text); } catch (e) { return null; }
}

function splitList(text) {
  return str(text).split(/[\s,，]+/).map(function (x) { return x.trim(); }).filter(Boolean);
}

function timeVal(v) {
  const m = /^(\d{1,2}):(\d{2})(?::\d{2})?$/.exec(str(v).trim());
  return m ? pad2(Number(m[1])) + ':' + m[2] : '';
}

function dateVal(v) {
  const m = /^(\d{4}-\d{2}-\d{2})/.exec(str(v).trim());
  return m ? m[1] : '';
}

function maskedLike(v) {
  return typeof v === 'string' && /^[\u2022*]{3,}$/.test(v.trim());
}

function announce(msg) {
  const el = $('live');
  if (el) el.textContent = str(msg);
}

function toast(kind, msg, ms) {
  const wrap = $('toasts');
  if (!wrap) return null;
  while (wrap.children.length >= 4) wrap.firstElementChild.remove();
  const el = document.createElement('div');
  el.className = 'toast ' + (kind || 'info');
  el.textContent = str(msg);
  wrap.append(el);
  const life = (ms === undefined) ? (kind === 'error' ? 9000 : 4500) : ms;
  if (life > 0) setTimeout(function () { el.remove(); }, life);
  return el;
}

function showError(boxId, err, hint) {
  const box = $(boxId);
  if (!box) return;
  const msg = (err && err.message) ? err.message : '未知错误';
  box.hidden = false;
  box.textContent = '';
  const head = document.createElement('p');
  head.className = 'err';
  head.textContent = msg;
  box.append(head);
  const details = (err && Array.isArray(err.details)) ? err.details : [];
  if (details.length) {
    const ul = document.createElement('ul');
    details.forEach(function (d) {
      const li = document.createElement('li');
      li.textContent = str(d);
      ul.append(li);
    });
    box.append(ul);
  }
  if (hint) {
    const p = document.createElement('p');
    p.className = 'muted small';
    p.textContent = hint;
    box.append(p);
  }
}

function clearBox(boxId) {
  const box = $(boxId);
  if (box) { box.hidden = true; box.textContent = ''; }
}

function showWarnings(boxId, list) {
  const box = $(boxId);
  if (!box) return;
  const arr = (Array.isArray(list) ? list : []).filter(function (x) { return str(x).trim() !== ''; });
  if (!arr.length) { box.hidden = true; box.textContent = ''; return; }
  box.hidden = false;
  box.textContent = '';
  const p = document.createElement('p');
  p.textContent = '服务端自动修正 / 提醒：';
  box.append(p);
  const ul = document.createElement('ul');
  arr.forEach(function (w) {
    const li = document.createElement('li');
    li.textContent = str(w);
    ul.append(li);
  });
  box.append(ul);
}

/* 忙碌期间按钮原有的子节点（含内嵌 <svg class="ico"> 图标）整体搬到 DocumentFragment 里
 * 存着，结束后原样搬回。为什么不把按钮内容序列化成一段标记塞进 dataset、回头再解析
 * 回来：① dataset 只能存字符串，节点身份（图标、已绑定过的监听器）会全丢；② 那样就得
 * 走「把字符串当标记写进文档」的路子，直接违反本仓库「动态内容一律 textContent」的约定
 * （两份验收测试都把它钉成了门禁）。这里只是搬家，不重建、不解析标记，输入天然是自家
 * 静态标记，没有注入面。 */
const BUSY_NODES = new WeakMap();

function setBusy(btn, text) {
  if (!btn) return;
  /* dataset.label 保留纯文本口径（既有排障/脚本可能读它），但不是还原依据 */
  if (!btn.dataset.label) btn.dataset.label = btn.textContent;
  if (!BUSY_NODES.has(btn)) {
    const stash = document.createDocumentFragment();
    while (btn.firstChild) stash.append(btn.firstChild);
    BUSY_NODES.set(btn, stash);
  }
  btn.dataset.busy = '1';
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  btn.textContent = '';
  const sp = document.createElement('span');
  sp.className = 'spinner';
  sp.setAttribute('aria-hidden', 'true');
  btn.append(sp, document.createTextNode(text || '处理中…'));
}

function clearBusy(btn) {
  if (!btn) return;
  const stash = BUSY_NODES.get(btn);
  if (stash) {
    /* 先还原内容、再解除 disabled / aria-busy：这样"可点击"的那一刻，按钮里已经是
       最终标记，不会出现「能点了但图标还没回来」的中间帧。 */
    BUSY_NODES.delete(btn);
    btn.textContent = '';
    btn.append(stash);
  } else if (btn.dataset.label) {
    btn.textContent = btn.dataset.label;
  }
  delete btn.dataset.busy;
  btn.disabled = false;
  btn.removeAttribute('aria-busy');
}

/* 类数组（NodeList / HTMLCollection / Array）统一遍历：不依赖 NodeList.prototype.forEach */
function each(list, fn) {
  if (!list) return;
  for (var i = 0; i < list.length; i++) fn(list[i], i);
}

/* 只对「本层才该提示」的状态码 toast，避免和 apiFetch 的通用提示重复 */
function toastLocal(e) {
  if (!e) return;
  const st = Number(e.status);
  if (st === 400 || st === 404 || st === 409 || st === 422) toast('error', e.message || ('请求失败（HTTP ' + st + '）'));
}

/* ---------- 3. apiFetch：统一 JSON / CSRF / 401 / 403 / 429 / 5xx ---------- */

class ApiError extends Error {
  constructor(status, code, message, details) {
    super(message || ('请求失败（HTTP ' + status + '）'));
    this.name = 'ApiError';
    this.status = Number(status) || 0;
    this.code = str(code);
    this.details = Array.isArray(details) ? details : [];
  }
}

async function apiFetch(path, opts) {
  opts = opts || {};
  const method = String(opts.method || 'GET').toUpperCase();
  const headers = { 'Accept': 'application/json' };
  const init = { method: method, credentials: 'same-origin', headers: headers, cache: 'no-store' };
  if (method !== 'GET' && method !== 'HEAD') headers['X-Requested-With'] = 'fzu-web';
  if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(opts.body);
  }

  let res;
  try {
    res = await fetch(API + path, init);
  } catch (e) {
    const err = new ApiError(0, 'network', '无法连接服务（进程是否已退出？）');
    if (!opts.quiet) toast('error', err.message);
    throw err;
  }

  let data = null;
  const ct = res.headers.get('Content-Type') || '';
  if (ct.indexOf('json') >= 0) {
    try { data = await res.json(); } catch (e) { data = null; }
  }
  if (res.ok) return data === null ? {} : data;

  const env = (data && data.error) ? data.error : {};
  const err = new ApiError(res.status, env.code, env.message, env.details);
  err.payload = data; // second_auth 这类 409 不走信封，原始体留着

  if (res.status === 401) {
    /* 测试登录失败也可能回 401，但那是「这条凭据不通」，不是管理端会话失效：
       keepSession 让调用方自己处理，别把用户踢回登录页。 */
    if (!opts.keepSession) {
      S.session.authed = false;
      showAuth('login');
    }
  } else if (res.status === 403) {
    if (/init|setup/i.test(err.code + err.message)) {
      await refreshSession().catch(function () {});
      showAuth('setup');
    } else if (!opts.quiet) {
      toast('warn', '请求被拒（403）：CSRF 校验未通过，请刷新页面后重试');
    }
  } else if (res.status === 429 && !opts.localRate) {
    /* localRate：这次的 429 是「这条短信刚发过/这个码刚试过」，调用方自己按住倒计时，
       别把它算进管理员登录的 15 分钟限速里（那会把登录框的按钮也锁掉）。 */
    rateLimit(res.headers.get('Retry-After'), err.message);
  } else if (res.status >= 500) {
    if (!opts.quiet) toast('error', err.message || ('服务内部错误（HTTP ' + res.status + '）'));
  }
  throw err;
}

function rateLimit(retryAfter, msg) {
  const sec = Math.max(1, Math.min(3600, Number(retryAfter) || 60));
  S.rateUntil = Date.now() + sec * 1000;
  toast('warn', msg || ('请求过于频繁，请 ' + sec + ' 秒后再试'), 8000);
  pumpRate();
  if (!S.rateTimer) S.rateTimer = setInterval(pumpRate, 1000);
}

function pumpRate() {
  const left = Math.ceil((S.rateUntil - Date.now()) / 1000);
  const el = $('auth-rate');
  const btn = $('login-submit');
  if (left <= 0) {
    if (S.rateTimer) { clearInterval(S.rateTimer); S.rateTimer = 0; }
    if (el) { el.hidden = true; el.textContent = ''; }
    if (btn && !btn.dataset.busy) btn.disabled = false;
    return;
  }
  if (el) {
    el.hidden = false;
    el.textContent = '已限速：' + left + ' 秒后可重试（同一 IP 15 分钟内失败 5 次触发）';
  }
  if (btn && !btn.dataset.busy) btn.disabled = true;
}

/* ---------- 4. 鉴权与路由 ---------- */

async function refreshSession() {
  const s = await apiFetch('/auth/session', { quiet: true });
  S.session = {
    initialized: !!(s && s.initialized),
    authed: !!(s && s.authed),
    env: (s && s.env) ? String(s.env) : ''
  };
  return S.session;
}

function resetData() {
  S.status = null; S.skew = 0; S.runs5 = []; S.slotsSig = ''; S.runsSig = '';
  S.autoBusy = false;
  S.config = null; S.configVersion = 0; S.configLoaded = false; S.importPreview = null; S.maskedFields = {};
  S.logs = []; S.logKeys = new Set(); S.logsSnapshotted = false;
  S.runs = []; S.runsPages = 1; S.runsExhausted = false;
}

function showAuth(mode) {
  stopDashboardLoop();
  closeStream();
  resetData();
  resetSecondAuthHints();
  smsCollapse();
  setHidden('shell', true);
  setHidden('booting', true);
  setHidden('auth', false);
  const setup = (mode === 'setup') || (mode === undefined && !S.session.initialized);
  setHidden('form-setup', !setup);
  setHidden('form-login', setup);
  setText('auth-sub', setup ? '首次使用 · 设置管理密码' : '请输入管理密码');
  setText('auth-env', S.session.env ? ('运行环境：' + S.session.env) : '');
  const focus = setup ? $('setup-pass') : $('login-pass');
  if (focus) setTimeout(function () { focus.focus(); }, 30);
}

/* 侧栏版本号跟着 /status 的 version 走：写死在 HTML 里迟早和二进制对不上，
   而运维核对版本时最先看的就是这个角落。 */
function applyVersion(st) {
  const v = str(st && st.version);
  if (v) setText('side-version', 'v' + v);
}

function enterApp() {
  S.session.authed = true;
  setHidden('auth', true);
  setHidden('booting', true);
  setHidden('file-notice', true);
  setHidden('shell', false);
  /* /status 只在仪表盘路由轮询；这里补一次，保证配置页/日志页/历史页也能看到版本号
     （失败就保持「版本 —」，不为一个展示字段打扰用户）。 */
  apiFetch('/status', { quiet: true }).then(applyVersion).catch(function () {});
  /* 控制台脚本交接过来的凭证：先落到配置页，再自动校验+保存 */
  const handoff = takeTokenFromHash();
  if (ROUTES.indexOf(location.hash) < 0) { location.hash = '#/dashboard'; return; }
  onRoute();
  if (handoff) applyHandoffToken(handoff);
}

/* ---------- 凭据交接：链接里的 #token=… ---------- */

/* 用户在自己浏览器里跑一段控制台脚本，把晚点名页面地址栏里的 token 直接交给面板，
   省掉"复制-粘贴"这一步。为什么只交接 token 而不是 Cookie：统一身份认证的 SESSION
   是 HttpOnly，任何页面脚本都读不到（document.cookie 里根本没有它），而晚点名落地页
   的地址栏里就写着 token，同源可读。 */
function takeTokenFromHash() {
  const m = /(?:^|[#&])token=([A-Za-z0-9]{8,})/.exec(location.hash || '');
  if (!m) return '';
  /* 立刻从地址栏抹掉：它是凭据，不该留在历史记录、截图或分享出去的链接里。
     replaceState 不发请求，所以这个 token 从来没有经过网络。 */
  history.replaceState(null, '', location.pathname + location.search + '#/config');
  return m[1];
}

async function applyHandoffToken(token) {
  await loadConfig(true);
  const cfg = JSON.parse(JSON.stringify(S.config || {}));
  cfg.user = cfg.user || {};
  cfg.user.token = token;
  tokenStatus('已从链接取得免登凭证，正在校验…', '');
  try {
    /* 先验证再保存：和「获取并保存凭证」同一条规矩，不把不能用的东西写进配置 */
    const probe = await apiFetch('/login/test', { method: 'POST', body: { token: token }, keepSession: true, quiet: true });
    const r = await apiFetch('/config', { method: 'PUT', body: { config: cfg } });
    if (r && Array.isArray(r.warnings) && r.warnings.length) showWarnings('config-warnings', r.warnings);
    S.configLoaded = false;
    await loadConfig(true);
    const parts = ['免登凭证已自动保存 ' + str(probe.token_hint)];
    if (str(probe.plan)) parts.push(str(probe.plan));
    tokenStatus(parts.join(' · '), 'ok');
    toast('ok', '免登凭证已自动保存');
    announce('已自动保存免登凭证');
  } catch (e) {
    setVal('cfg-token', token);
    /* 只取第一行：校验失败时 e.message 后面会跟一大段逐条规则，塞进状态行就没法看了。
       凭证已经填进输入框，用户补完其它项点「保存」即可，不用再跑一遍脚本。 */
    const first = String((e && e.message) || '保存失败').split('\n')[0];
    tokenStatus('已取到凭证但没能自动保存：' + first + '（凭证已填进「免登凭证」框，补完其它项后点「保存」即可）', 'err');
  }
}

/* 面板已经开着时再贴链接，只会发生同文档的 hash 变化（页面不重载）——
   这条路也必须接住，否则用户"在已打开的标签里粘链接"就没反应。 */
window.addEventListener('hashchange', function () {
  if (!S.session.authed) return;
  const later = takeTokenFromHash();
  if (!later) return;
  onRoute();
  applyHandoffToken(later);
});

function onRoute() {
  if (!S.session.authed) { showAuth('login'); return; }
  const hash = ROUTES.indexOf(location.hash) >= 0 ? location.hash : '#/dashboard';
  if (hash !== location.hash) { location.hash = hash; return; }

  S.route = hash;
  ROUTES.forEach(function (r) {
    setHidden('view-' + r.slice(2), r !== hash);
  });
  each(document.querySelectorAll('.nav-btn'), function (b) {
    if (b.dataset.route === hash) b.setAttribute('aria-current', 'page');
    else b.removeAttribute('aria-current');
  });

  if (hash !== '#/dashboard') stopDashboardLoop();
  if (hash !== '#/logs') closeStream();

  if (hash === '#/dashboard') startDashboardLoop();
  else if (hash === '#/config') loadConfig(false);
  else if (hash === '#/logs') startLogs();
  else if (hash === '#/runs') loadRuns(true);
}

/* ---------- 5. 仪表盘 ---------- */

function startDashboardLoop() {
  stopDashboardLoop();
  loadDashboard(S.status === null);
  S.statusTimer = setInterval(function () {
    if (S.route === '#/dashboard') loadDashboard(true);
  }, STATUS_EVERY_MS);
  S.tickTimer = setInterval(tickCountdown, 1000);
  tickCountdown();
}

function stopDashboardLoop() {
  if (S.statusTimer) { clearInterval(S.statusTimer); S.statusTimer = 0; }
  if (S.tickTimer) { clearInterval(S.tickTimer); S.tickTimer = 0; }
}

async function loadDashboard(showToast) {
  const quiet = !showToast;
  try {
    const pair = await Promise.all([
      apiFetch('/status', { quiet: quiet }),
      apiFetch('/runs?limit=5', { quiet: quiet })
    ]);
    const st = pair[0] || {};
    S.status = st;
    const serverNow = toMs(st.now);
    S.skew = Number.isFinite(serverNow) ? serverNow - Date.now() : 0;
    S.runs5 = (pair[1] && Array.isArray(pair[1].runs)) ? pair[1].runs : [];
    patchDashboard();
    tickCountdown();
    setText('dash-live', '状态更新于 ' + fmtTime(Date.now() + S.skew));
  } catch (e) {
    setText('dash-live', '状态刷新失败：' + (e.message || '未知错误') + '（15 秒后重试）');
  }
}

function vacationLabel(v) {
  if (!v) return '';
  if (typeof v === 'string') return v;
  const name = str(v.name) || '假期区间';
  const range = [str(v.start), str(v.end)].filter(Boolean).join(' ~ ');
  return range ? (name + '（' + range + '）') : name;
}

function decisionText(d) {
  const s = str(d);
  if (!s) return '—';
  if (s.indexOf('force:') === 0) return '手动强制 · ' + (DECISION_TEXT[s.slice(6)] || s.slice(6));
  return DECISION_TEXT[s] || s;
}

function schedulerText(s) {
  if (!s) return '—';
  if (s.stopped) return '已停止（不再自动执行）';
  if (s.running_now) return '正在执行一次签到';
  if (s.enabled === false) return '未启用';
  return '空闲，等待下一档';
}

/* ---------- 5.1 自动签到总开关（paused + POST /control） ---------- */

/* paused 只认布尔，没给就当不可用 */
function pausedOf(sched) {
  return (sched && typeof sched.paused === 'boolean') ? sched.paused : null;
}

/* 只画不发请求；busy = 乐观更新 */
function paintAutoSwitch(sched, busy) {
  const btn = $('btn-auto-switch');
  if (!btn) return;
  const paused = pausedOf(sched);
  btn.setAttribute('aria-checked', paused === false ? 'true' : 'false');
  btn.disabled = (paused === null) || !!busy || S.autoBusy;
  setText('auto-power-state', '自动签到 · ' + (paused === null ? '状态不可用，切不了'
    : (paused ? '已暂停 · 手动「立即签到」照旧' : '已开启，到档位自动签到')));
}

async function toggleAutoSwitch() {
  const from = pausedOf((S.status || {}).scheduler);
  if (S.autoBusy || from === null) return;
  S.autoBusy = true;
  paintAutoSwitch({ paused: !from }, true);
  try {
    /* quiet：下面那条才有「已回到原样」，别让 5xx 的通用 toast 重复一遍 */
    const r = await apiFetch('/control', { method: 'POST', body: { paused: !from }, keepSession: true, quiet: true });
    S.autoBusy = false;
    if (S.status && r && r.scheduler) S.status.scheduler = r.scheduler;
    paintAutoSwitch(r && r.scheduler ? r.scheduler : { paused: !from });
    toast('ok', from ? '已恢复自动签到' : '已暂停自动签到');
  } catch (e) {
    S.autoBusy = false;
    paintAutoSwitch({ paused: from }); // 回滚到点击前
    toast('error', '没能切换：' + (e.message || ('HTTP ' + e.status)) + '，已回到原样', 12000);
  }
  loadDashboard(false); // 以服务端为准
}

/* 推送通道的内部名字（serverchan / pushplus …）只在配置里出现，界面上给中文名 */
function notifyName(v) {
  const s = str(v);
  return NOTIFY_NAME[s] || s || NOTIFY_NAME.none;
}

/* 执行方式同理：normal / force 是接口字段，人只看「自动」还是「手动」 */
function modeText(v) {
  const s = str(v);
  return MODE_TEXT[s] || s || '—';
}

function patchDashboard() {
  const st = S.status || {};
  const day = st.day || {};
  const plan = day.plan || {};
  const win = st.window || {};
  const sched = st.scheduler || {};
  const slots = Array.isArray(st.slots) ? st.slots : [];
  const vac = st.vacation;

  const checked = day.checked_in === true;
  const chip = $('dash-check');
  if (chip) {
    chip.textContent = checked ? '今日已签' : '今日待签';
    chip.className = 'chip ' + (checked ? 'ok' : 'warn');
  }

  const winEl = $('dash-window');
  if (winEl) {
    winEl.textContent = (win.inside ? '窗口内 ' : '窗口外 ') + (str(win.start) || '--:--') + '–' + (str(win.end) || '--:--');
    winEl.className = 'chip ' + (win.inside ? 'ok' : '');
  }

  const vacEl = $('dash-vacation');
  if (vacEl) {
    if (vac) { vacEl.hidden = false; vacEl.textContent = '假期中：' + vacationLabel(vac); }
    else { vacEl.hidden = true; vacEl.textContent = ''; }
  }

  let summary;
  if (vac) summary = '假期区间内：调度器不签到、不推送，也不留运行记录。';
  else if (checked) summary = '今日已签到' + (day.last_success_date ? '（最近成功：' + str(day.last_success_date) + '）' : '') + '，无需重复操作。';
  else if (plan.has_plan !== true) summary = '今日无考勤计划：可能计划还没发布，稍后再看。';
  else if (win.inside) summary = '在签到窗口内，等待下一次目标时刻自动打卡。';
  else summary = '当前不在签到窗口内（' + (str(win.start) || '--:--') + '–' + (str(win.end) || '--:--') + '），到窗口后自动执行。';
  setText('dash-summary', summary);

  setText('dash-date', str(day.date) || '—');
  setText('dash-last-success', str(day.last_success_date) || '—');
  setText('dash-decision', decisionText(sched.last_decision));
  setText('dash-scheduler', schedulerText(sched));
  if (!S.autoBusy) paintAutoSwitch(sched);
  /* 只留服务版本号：配置版本不再对用户露出（/status 里仍带着 config_version，排障用） */
  setText('dash-version', 'v' + (str(st.version) || '—'));
  applyVersion(st);

  const hasPlan = plan.has_plan === true;
  setHidden('dash-plan', !hasPlan);
  setHidden('dash-plan-empty', hasPlan);
  setText('plan-start', str(plan.start) || '—');
  setText('plan-end', str(plan.end) || '—');
  setText('plan-late', str(plan.late_line) || '—');

  const sig = slots.map(function (s) {
    return [s.configured, s.target, s.state, s.jittered ? 1 : 0, s.clamped ? 1 : 0, s.reason].join(',');
  }).join('|');
  if (sig !== S.slotsSig) { S.slotsSig = sig; renderSlots(slots); }
  setHidden('dash-slots-empty', slots.length > 0);
  setText('dash-slot-count', slots.length ? '共 ' + slots.length + ' 档' : '');

  renderRecentRuns();
  updateActionState(sched);
}

function updateActionState(sched) {
  const btn = $('btn-force');
  if (!btn || btn.dataset.busy) return;
  const running = !!(sched && sched.running_now);
  btn.disabled = running;
  btn.title = running ? '已有一次运行在进行中' : '跳过等待与窗口外拦截，立刻执行一次签到';
}

function tag(text) {
  const el = document.createElement('span');
  el.className = 'tag';
  el.textContent = text;
  return el;
}

function renderSlots(slots) {
  const list = $('dash-slots');
  if (!list) return;
  list.textContent = '';
  slots.forEach(function (s) {
    const state = SLOT_TEXT[s.state] ? s.state : 'pending';
    const li = document.createElement('li');
    li.className = 'st-' + state;
    const times = document.createElement('span');
    times.className = 'times';
    times.textContent = (str(s.configured) || '--:--') + ' → ' + (str(s.target) || '--:--:--');
    const chip = document.createElement('span');
    chip.className = 'chip ' + (state === 'done' ? 'ok' : (state === 'missed' ? 'err' : 'warn'));
    chip.textContent = SLOT_TEXT[state];
    li.append(times, chip);
    if (s.jittered) li.append(tag('抖动'));
    if (s.clamped) li.append(tag('已钳制'));
    if (s.valid === false) li.append(tag('窗口外'));
    if (s.reason && s.reason !== 'ok') {
      const r = document.createElement('span');
      r.className = 'muted small';
      r.textContent = str(s.reason);
      li.append(r);
    }
    list.append(li);
  });
}

function renderRecentRuns() {
  const body = $('dash-runs-body');
  if (!body) return;
  setHidden('dash-runs-empty', S.runs5.length > 0);
  const sig = S.runs5.map(function (r) { return str(r && r.id); }).join('|');
  if (sig === S.runsSig) return;
  S.runsSig = sig;
  body.textContent = '';
  S.runs5.forEach(function (r) {
    const tr = document.createElement('tr');
    const t1 = document.createElement('td');
    t1.className = 'mono';
    t1.textContent = fmtDateTime(r && r.started_at);
    const t2 = document.createElement('td');
    const chip = document.createElement('span');
    const kind = STATUS_KIND[r && r.status] || '';
    chip.className = 'chip' + (kind ? ' ' + kind : '');
    chip.textContent = STATUS_TEXT[r && r.status] || str(r && r.status) || '—';
    t2.append(chip);
    const t3 = document.createElement('td');
    t3.className = 'mono';
    t3.textContent = fmtLatency(r && r.latency_ms);
    const t4 = document.createElement('td');
    t4.className = (r && r.error) ? 'mono' : 'mono muted';
    const errText = str(r && r.error);
    t4.textContent = errText ? (errText.length > 120 ? errText.slice(0, 120) + '…' : errText) : '—';
    tr.append(t1, t2, t3, t4);
    body.append(tr);
  });
}

function tickCountdown() {
  const el = $('dash-countdown');
  if (!el) return;
  const st = S.status || {};
  const target = toMs(st.next_run_at);
  if (!Number.isFinite(target) || new Date(target).getFullYear() < 2000) {
    el.textContent = '--:--:--';
    el.classList.remove('is-urgent');
    setText('dash-next-at', '没有后续档位（今日计划已结束，或者设置的签到时刻都落在签到窗口之外）');
    return;
  }
  const now = Date.now() + (S.skew || 0);
  const left = Math.max(0, Math.floor((target - now) / 1000));
  /* 最后一分钟给数字一点呼吸：这是整页唯一会自己变的数，动起来才有人盯得住 */
  if (left > 0 && left <= 60) el.classList.add('is-urgent');
  else el.classList.remove('is-urgent');
  el.textContent = pad2(Math.floor(left / 3600)) + ':' + pad2(Math.floor((left % 3600) / 60)) + ':' + pad2(left % 60);
  setText('dash-next-at', '目标 ' + fmtTime(target) + (left === 0 ? '（已到点，等待执行结果）' : ''));
  if (left === 0) {
    const t = Date.now();
    if (!S.zeroGuard || t - S.zeroGuard > 5000) { S.zeroGuard = t; loadDashboard(true); }
  }
}

async function startRun(mode, dryRun, btn, label) {
  setBusy(btn, label);
  setText('dash-action-hint', '已提交，服务正在执行…');
  try {
    const res = await apiFetch('/runs', { method: 'POST', body: { mode: mode, dry_run: !!dryRun } });
    const id = str(res && res.run_id);
    const what = dryRun ? '定位预检' : '签到';
    toast('ok', what + '已受理' + (id ? '（运行编号 ' + id + '）' : '') + '，结果见日志与推送');
    announce(what + '已受理，正在执行');
    setText('dash-action-hint', '已受理' + (id ? '：' + id : '') + '，稍后自动刷新结果…');
    setTimeout(function () { loadDashboard(true); }, 2000);
    setTimeout(function () { loadDashboard(true); }, 9000);
  } catch (e) {
    let msg = e.status === 409 ? '已有一次运行正在进行中，等它结束后再试。' : ('提交失败：' + (e.message || ('HTTP ' + e.status)));
    if (isSecondAuth(e)) {
      msg = '学校要求短信二次验证，这次没签：去配置页「账号」里点「打开登录弹窗」，用学号密码补一条验证码，或换一份免登凭证。';
      toast('error', str(e.payload.message) || '学校要求短信二次验证', 12000);
    } else {
      toastLocal(e);
    }
    setText('dash-action-hint', msg);
  } finally {
    clearBusy(btn);
    updateActionState((S.status || {}).scheduler);
  }
}

/* ---------- 6. 配置 ---------- */

async function loadConfig(force) {
  if (S.configLoaded && !force) return;
  try {
    const d = await apiFetch('/config');
    S.config = (d && d.config) ? d.config : {};
    S.maskedFields = (d && d.masked_fields) ? d.masked_fields : {};
    // 配置不再有版本号：这里也不再留一个 configVersion 的镜像，省得再把 version 塞回请求体。
    S.configLoaded = true;
    fillConfig(S.config);
    clearBox('config-msg');
    showWarnings('config-warnings', (d && d.warnings) || []);
    /* 只报来源，不报版本号：/config 的响应已不带 version，界面上再拼就是假信息 */
    setText('config-version', (d && d.source) ? '来源 ' + str(d.source) : '');
  } catch (e) {
    if (e.status !== 401) showError('config-msg', e, '配置读取失败：确认服务在运行且已登录。');
  }
}

function fillConfig(cfg) {
  const user = cfg.user || {};
  const ck = cfg.checkin || {};
  const sch = ck.schedule || {};
  const vac = cfg.vacation || {};
  const nt = cfg.notify || {};
  const srv = cfg.server || {};

  setVal('cfg-username', user.username);
  setVal('cfg-password', user.password);
  setVal('cfg-token', user.token);
  /* 弹窗里那份只是这两项的镜像：值从服务端来，别让两边各存一份真相 */
  syncLoginInputs();

  setVal('cfg-lon', ck.longitude);
  setVal('cfg-lat', ck.latitude);
  setVal('cfg-actual', ck.actual_location);
  setVal('cfg-jitter', numStr(ck.location_jitter_m));

  setVal('cfg-window-start', timeVal(sch.window_start));
  setVal('cfg-window-end', timeVal(sch.window_end));
  setVal('cfg-jitter-min', numStr(sch.time_jitter_minutes));
  setVal('cfg-maxwait', numStr(sch.max_wait_minutes));
  setVal('cfg-tolerance', numStr(sch.tolerance_minutes));

  const cb = $('cfg-vacation-notify');
  if (cb) cb.checked = vac.notify === true;

  setVal('cfg-notify-type', nt.type || 'none');
  setVal('cfg-serverchan-key', nt.serverchan_key);
  setVal('cfg-pushplus-token', nt.pushplus_token);
  setVal('cfg-bark-url', nt.bark_url);
  setVal('cfg-wecom-webhook', nt.wecom_webhook);

  setVal('cfg-listen', srv.listen);
  setVal('cfg-datadir', srv.data_dir);
  setVal('cfg-origins', Array.isArray(srv.allow_origins) ? srv.allow_origins.join(', ') : srv.allow_origins);

  const lbox = $('pool-rows');
  if (lbox) lbox.textContent = '';
  const locs = Array.isArray(ck.locations) ? ck.locations : [];
  if (locs.length) locs.forEach(function (l) { addPoolRow(l || {}); });
  else addPoolRow({});

  const tbox = $('time-rows');
  if (tbox) tbox.textContent = '';
  const times = Array.isArray(sch.times) ? sch.times : [];
  if (times.length) times.forEach(function (t) { addTimeRow({ time: t }); });
  else addTimeRow({});

  const vbox = $('vac-rows');
  if (vbox) vbox.textContent = '';
  const ranges = Array.isArray(vac.skip_ranges) ? vac.skip_ranges : [];
  ranges.forEach(function (r) { addVacRow(r || {}); });

  /* 重新拉回来的配置里凭据是哨兵：所有「眼睛」按钮回到隐藏态，
     否则会出现 input.type=text 里显示四个圆点的错觉（看起来像密码本身）。
     类型和值一起复位 = 一个干净的初始态。 */
  each(document.querySelectorAll('.peek'), function (btn) { setPeek(btn, false); });

  syncPoolNote();
  applyNotifyType();
  applySecretBadges();
  syncCredNote();
  syncAccountSummary();
  syncCookieNote();
}

function addPoolRow(loc) {
  const box = $('pool-rows');
  const tpl = $('tpl-pool-row');
  if (!box || !tpl) return;
  const node = tpl.content.firstElementChild.cloneNode(true);
  node.querySelector('.r-lon').value = str(loc.longitude);
  node.querySelector('.r-lat').value = str(loc.latitude);
  node.querySelector('.r-actual').value = str(loc.actual_location);
  node.querySelector('.r-weight').value = numStr(loc.weight);
  box.append(node);
}

function addTimeRow(item) {
  const box = $('time-rows');
  const tpl = $('tpl-time-row');
  if (!box || !tpl) return;
  const node = tpl.content.firstElementChild.cloneNode(true);
  node.querySelector('.t-time').value = timeVal(item && item.time);
  box.append(node);
}

function addVacRow(item) {
  const box = $('vac-rows');
  const tpl = $('tpl-vac-row');
  if (!box || !tpl) return;
  const node = tpl.content.firstElementChild.cloneNode(true);
  node.querySelector('.v-name').value = str(item && item.name);
  node.querySelector('.v-start').value = dateVal(item && item.start);
  node.querySelector('.v-end').value = dateVal(item && item.end);
  box.append(node);
}

function readPool() {
  const box = $('pool-rows');
  if (!box) return [];
  const out = [];
  each(box.querySelectorAll('.row'), function (row) {
    const lon = row.querySelector('.r-lon').value.trim();
    const lat = row.querySelector('.r-lat').value.trim();
    const act = row.querySelector('.r-actual').value.trim();
    const w = row.querySelector('.r-weight').value.trim();
    if (!lon && !lat && !act && !w) return;
    out.push({ longitude: lon, latitude: lat, actual_location: act, weight: w ? numOrNull(w) : null });
  });
  return out;
}

function readTimes() {
  const box = $('time-rows');
  if (!box) return [];
  const out = [];
  each(box.querySelectorAll('.t-time'), function (inp) {
    const v = timeVal(inp.value);
    if (v && out.indexOf(v) < 0) out.push(v);
  });
  return out;
}

function readVac() {
  const box = $('vac-rows');
  if (!box) return [];
  const out = [];
  each(box.querySelectorAll('.row'), function (row) {
    const name = row.querySelector('.v-name').value.trim();
    const start = row.querySelector('.v-start').value.trim();
    const end = row.querySelector('.v-end').value.trim();
    if (!name && !start && !end) return;
    out.push({ name: name, start: start, end: end });
  });
  return out;
}

function poolRowFilled(row) {
  // 判「这一行算不算配了位置」的规则必须与 readPool 完全一致：模板行是空壳，
  // 只是给用户一个可填的槽位，不能因为它存在就宣称"位置池已接管单点"。
  const val = function (sel) {
    const el = row.querySelector(sel);
    return el ? String(el.value || '').trim() : '';
  };
  return !!(val('.r-lon') || val('.r-lat') || val('.r-actual') || val('.r-weight'));
}

function syncPoolNote() {
  const n = Array.prototype.filter.call(
    document.querySelectorAll('#pool-rows .row'),
    poolRowFilled
  ).length;
  const note = $('cfg-single-note');
  const fs = $('fs-location');
  if (n > 0) {
    if (note) {
      note.hidden = false;
      note.textContent = '位置池已有 ' + n + ' 项：上面的单个经度 / 纬度 / 地点文字已被位置池接管，不再生效（每次按各点填的权重随机挑一个点）。';
    }
    if (fs) fs.classList.add('superseded');
  } else {
    // 只有空壳行（或一行都没有）：单点仍然是生效来源，别把用户吓到以为配置失效。
    if (note) { note.hidden = true; note.textContent = ''; }
    if (fs) fs.classList.remove('superseded');
  }
}

function applyNotifyType() {
  const sel = $('cfg-notify-type');
  if (!sel) return;
  const type = sel.value || 'none';
  each(document.querySelectorAll('[data-notify]'), function (el) {
    el.hidden = el.dataset.notify !== type;
  });
  const hint = $('notify-hint');
  if (hint) hint.textContent = NOTIFY_HINT[type] || NOTIFY_HINT.none;
}

function applySecretBadges() {
  each(document.querySelectorAll('.has-secret'), function (wrap) {
    /* password / token / 推送密钥都是 input，弹窗里那份也在其中 */
    const input = wrap.querySelector('input, textarea');
    const badge = wrap.querySelector('.mask-badge');
    if (!input || !badge) return;
    badge.hidden = !(input.value.trim() !== '' || S.maskedFields[wrap.dataset.secret]);
  });
}

/* 凭据全空也能保存：只提示，不进 validateLocal */
function syncCredNote() {
  const el = $('cfg-cred-note');
  if (!el) return;
  const empty = ['cfg-username', 'cfg-password', 'cfg-token'].every(function (id) { return !getVal(id); });
  el.hidden = !empty;
  el.textContent = empty ? '凭据留空 = 只保存其它设置，不会签到；补上学号密码或免登凭证后才会自动签到。' : '';
}

/* ---------- 6.0 登录弹窗：学号密码与短信码都在弹窗里，配置页只留一行摘要 ---------- */

/* 弹窗输入框只是 #cfg-username / #cfg-password 两个隐藏域的可视镜像：
   保存仍然读隐藏域，所以「在弹窗里填完不点保存」不会悄悄改动配置。 */
function syncLoginInputs() {
  setVal('lg-username', getVal('cfg-username'));
  setVal('lg-password', getVal('cfg-password'));
}

/* 摘要只说配没配，不显示学号：这一行最容易被截图贴出去问问题 */
function syncAccountSummary() {
  const el = $('account-summary');
  if (!el) return;
  const user = getVal('cfg-username');
  const pass = getVal('cfg-password');
  const text = (user && pass) ? '已填学号 + 密码' : user ? '只填了学号' : pass ? '只填了密码' : '未配置';
  el.textContent = text;
  el.className = 'account-summary' + ((user || pass) ? '' : ' miss');
}

/* Cookie 的填写栏已经去掉：老配置里那份不再显示，但默认原样保留（flow 还会拿它
   当备用）。这里只在确实存着一份时给一条明确的出口，免得有人想删却删不掉。 */
function syncCookieNote() {
  const note = $('cookie-note');
  if (!note) return;
  const saved = !!(((S.config && S.config.user) || {}).cookie);
  note.hidden = !(saved && !S.dropCookie);
}

/* 弹窗里改了什么就同步回隐藏域 + 徽标 + 摘要，三处永远一致 */
function mirrorAccount() {
  setVal('cfg-username', getVal('lg-username'));
  setVal('cfg-password', getVal('lg-password'));
  syncAccountSummary();
  applySecretBadges();
  syncCredNote();
}

function openLoginModal() {
  syncLoginInputs();
  showOverlay('modal-login');
  if (smsIsOpen()) { smsFocusCode(); return; }
  /* 同步聚焦：hidden 去掉之后元素立刻可聚焦，没必要等一帧——等一帧反而会让人
     先看到"没有光标"的一瞬，自动化里也会读到还没落位的焦点 */
  const el = $(getVal('lg-username') ? 'lg-password' : 'lg-username');
  if (el) { el.focus(); if (el.select) el.select(); }
}

function closeLoginModal() { setHidden('modal-login', true); }

/* 换到凭证后要重拉配置，但服务端只写 token：弹窗里刚打的账密不能被刷回旧值，
   否则「验证码过了 → 学号密码凭空消失 → 保存后什么都没存」。 */
function reloadConfigKeepAccount() {
  const user = getVal('lg-username');
  const pass = getVal('lg-password');
  S.configLoaded = false;
  return loadConfig(true).then(function () {
    if (user) { setVal('lg-username', user); setVal('cfg-username', user); }
    if (pass) { setVal('lg-password', pass); setVal('cfg-password', pass); }
    syncAccountSummary();
    applySecretBadges();
    syncCredNote();
  }, function () { /* 刷新失败不影响这次登录结论 */ });
}

/* 眼睛按钮：只改 input.type，value 不动 */
function setPeek(btn, plain) {
  const input = $(btn.getAttribute('aria-controls'));
  if (!input) return;
  input.type = plain ? 'text' : 'password';
  btn.setAttribute('aria-pressed', plain ? 'true' : 'false');
  const label = (plain ? '隐藏' : '显示') + (btn.dataset.name || '');
  btn.setAttribute('aria-label', label);
  btn.title = label;
}

/* 眼睛按钮的完整行为：
 *   - 已经显示（aria-pressed=true）→ 收回去；
 *   - 框里是刚打的明文（不是哨兵）→ 只切 type，不打扰服务端；
 *   - 是哨兵（已配置，页面不再持有真实值）→ 弹窗二次校验管理密码，取回**一项**。
 * 第三种是这次改版的重点：读取路径不再明文回显，所以"看一眼"必须是一个显式动作，
 * 而且要能证明人就在跟前（7 天的会话不够）。 */
async function peekSecret(btn) {
  const input = $(btn.getAttribute('aria-controls'));
  if (!input) return;
  if (btn.getAttribute('aria-pressed') === 'true') { setPeek(btn, false); return; }
  if (input.value && input.value !== MASK_SENTINEL) { setPeek(btn, true); return; }
  openReveal(btn);
}

function openReveal(btn) {
  const input = $(btn.getAttribute('aria-controls'));
  const wrap = (btn.closest && btn.closest('.has-secret')) ? btn.closest('.has-secret') : null;
  const field = wrap ? str(wrap.dataset.secret) : '';
  if (!input || !field) { setPeek(btn, true); return; } /* 没有字段路径就退化成单纯显示 */
  S.pendingReveal = { btn: btn, input: input, field: field, label: str(btn.dataset.name) || field };
  setText('reveal-field', S.pendingReveal.label);
  clearBox('reveal-err');
  const pass = $('reveal-pass');
  if (pass) pass.value = '';
  /* 从弹窗里的眼睛点进来时，这一层必须落在弹窗之上（见 showOverlay 的注释） */
  showOverlay('modal-reveal');
  if (pass) setTimeout(function () { pass.focus(); }, 30);
}

function closeReveal() {
  setHidden('modal-reveal', true);
  S.pendingReveal = null;
  const pass = $('reveal-pass');
  if (pass) pass.value = '';
  clearBox('reveal-err');
}

async function submitReveal(btn) {
  const pending = S.pendingReveal;
  if (!pending) return;
  const pass = $('reveal-pass');
  const password = pass ? String(pass.value) : '';
  clearBox('reveal-err');
  if (!password) { showError('reveal-err', { message: '请输入管理密码' }, ''); return; }
  setBusy(btn, '校验中…');
  try {
    const d = await apiFetch('/config/reveal', {
      method: 'POST',
      body: { field: pending.field, password: password },
      /* keepSession：这个接口的 401 是「管理密码输错了」，不是登录失效。
         走默认的 401 处理会把用户踢回登录页——输错一次赔上一整张面板，太贵。 */
      keepSession: true
    });
    pending.input.value = str(d && d.value);
    setPeek(pending.btn, true);
    closeReveal();
    toast('info', '已显示' + pending.label + '：不动它直接保存也不会改变内容');
  } catch (e) {
    showError('reveal-err', e,
      (e && e.status === 429) ? '同一 IP 15 分钟内最多试 5 次，等一会儿再来。' : '连续输错会按 IP 限速，与登录共用同一套阈值。');
  } finally {
    clearBusy(btn);
  }
}

function collectConfig() {
  const cur = S.config || {};
  return {
    user: {
      username: getVal('cfg-username'),
      password: getVal('cfg-password'),
      token: getVal('cfg-token'),
      /* Cookie 已经没有填写栏：默认把服务端那份原样带回去（哨兵值 = 保持原值，
         MergeMasked 认得），只有点过「清掉它」才真的清空。配置没读到时也发哨兵——
         那表示"我不知道你现在存着什么"，比发空串（等于删掉）安全。 */
      cookie: S.dropCookie ? '' : (!S.config ? MASK_SENTINEL : str((S.config.user || {}).cookie))
    },
    checkin: {
      longitude: getVal('cfg-lon'),
      latitude: getVal('cfg-lat'),
      actual_location: getVal('cfg-actual'),
      locations: readPool(),
      location_jitter_m: numOrNull(getVal('cfg-jitter')),
      schedule: {
        window_start: getVal('cfg-window-start'),
        window_end: getVal('cfg-window-end'),
        times: readTimes(),
        time_jitter_minutes: numOrNull(getVal('cfg-jitter-min')),
        max_wait_minutes: numOrNull(getVal('cfg-maxwait')),
        tolerance_minutes: numOrNull(getVal('cfg-tolerance'))
      }
    },
    vacation: {
      notify: !!($('cfg-vacation-notify') || {}).checked,
      skip_ranges: readVac()
    },
    notify: {
      type: getVal('cfg-notify-type') || 'none',
      serverchan_key: getVal('cfg-serverchan-key'),
      pushplus_token: getVal('cfg-pushplus-token'),
      bark_url: getVal('cfg-bark-url'),
      wecom_webhook: getVal('cfg-wecom-webhook')
    },
    server: {
      listen: getVal('cfg-listen'),
      data_dir: getVal('cfg-datadir'),
      allow_origins: splitList(getVal('cfg-origins'))
    }
  };
}

function markInvalid(id, bad) {
  const el = $(id);
  if (!el) return;
  if (bad) el.setAttribute('aria-invalid', 'true');
  else el.removeAttribute('aria-invalid');
}

function validateLocal(cfg) {
  const p = [];
  const ck = cfg.checkin;
  const pool = ck.locations;
  const hasPool = pool.length > 0;
  const lon = ck.longitude;
  const lat = ck.latitude;

  if (!hasPool && (!lon || !lat)) p.push('没有可用坐标：填单点「经度 + 纬度」，或在位置池里加一项。');
  const badLon = !hasPool && !!lon && !Number.isFinite(Number(lon));
  const badLat = !hasPool && !!lat && !Number.isFinite(Number(lat));
  if (badLon) p.push('经度不是数字：' + lon);
  if (badLat) p.push('纬度不是数字：' + lat);
  markInvalid('cfg-lon', badLon);
  markInvalid('cfg-lat', badLat);

  pool.forEach(function (l, i) {
    if (!l.longitude || !l.latitude) p.push('位置池第 ' + (i + 1) + ' 项缺经度或纬度。');
    else if (!Number.isFinite(Number(l.longitude)) || !Number.isFinite(Number(l.latitude))) {
      p.push('位置池第 ' + (i + 1) + ' 项的经纬度不是数字。');
    }
  });

  const ws = cfg.checkin.schedule.window_start;
  const we = cfg.checkin.schedule.window_end;
  const badWin = !!ws && !!we && ws >= we;
  if (badWin) p.push('窗口终点必须晚于起点（不支持跨日）：' + ws + ' → ' + we);
  markInvalid('cfg-window-end', badWin);

  cfg.vacation.skip_ranges.forEach(function (r, i) {
    if (!r.start || !r.end) p.push('假期第 ' + (i + 1) + ' 段缺起始或结束日期。');
    else if (r.start > r.end) p.push('假期第 ' + (i + 1) + ' 段起始晚于结束：' + r.start + ' → ' + r.end);
  });

  /* 凭据不参与拦截：全留空也能存，提示走 syncCredNote */
  return p;
}

async function saveConfig() {
  const btn = $('btn-config-save');
  clearBox('config-msg');
  const cfg = collectConfig();
  const problems = validateLocal(cfg);
  if (problems.length) {
    showError('config-msg', { message: '有 ' + problems.length + ' 处需要先修正', details: problems }, '改完再点「保存」；坐标可在高德坐标拾取器选点后填进「经度」「纬度」。');
    return;
  }
  setBusy(btn, '保存中…');
  try {
    const r = await apiFetch('/config', { method: 'PUT', body: { config: cfg } });
    S.config = cfg;
    S.configLoaded = true;
    await loadConfig(true);
    if (r && Array.isArray(r.warnings) && r.warnings.length) showWarnings('config-warnings', r.warnings);
    toast('ok', '配置已保存');
    announce('配置已保存');
    loadDashboard(true);
  } catch (e) {
    showError('config-msg', e, '按上面逐条提示修改后再保存；错误原文来自服务端校验（与 YAML 导入同一套规则）。');
  } finally {
    clearBusy(btn);
  }
}

/* ---------- 6.0 改管理密码：POST /auth/password ---------- */

/* 这是"登录本面板"的密码，与学号密码无关。成功之后服务端会换会话代次：
 * 包括本页在内的所有已登录设备都拿到新代次，旧 Cookie 立即作废（本页由响应里的
 * Set-Cookie 自动续上）。 */
async function changePassword(btn) {
  const cur = getVal('pwd-current');
  const next = getVal('pwd-new');
  clearBox('pwd-msg');
  if (!cur) { showError('pwd-msg', { message: '先填当前管理密码' }, ''); return; }
  if (next.length < 8) { showError('pwd-msg', { message: '新密码至少 8 个字符' }, '口令我建议用一句长短语，比 8 位随机字符好记也更强。'); return; }
  if (next === cur) { showError('pwd-msg', { message: '新密码与当前密码相同，等于没改' }, ''); return; }
  setBusy(btn, '提交中…');
  try {
    await apiFetch('/auth/password', {
      method: 'POST',
      body: { current: cur, new: next },
      /* 这里的 401 是「当前密码不对」，不是会话失效 */
      keepSession: true
    });
    setVal('pwd-current', '');
    setVal('pwd-new', '');
    toast('ok', '管理密码已修改：其它设备上的登录已全部失效');
    announce('管理密码已修改');
  } catch (e) {
    showError('pwd-msg', e, (e && e.status === 429) ? '失败次数太多，等限速窗口过去再试。' : '连续输错会按 IP 限速，和登录共用同一套阈值。');
  } finally {
    clearBusy(btn);
  }
}

/* ---------- 6.1 「测试登录」：POST /login/test ---------- */

const LOGIN_MODE_TEXT = { cookie: 'Cookie', password: '学号 + 密码', token: '免登凭证' };

/* 弹窗里只有学号密码两项，所以这里**只**发这两项：token 在配置页，由「验证这份
   凭证」单独测。混在一起发过一次的坑很具体 —— 服务端按 cookie → token → 账密
   的顺序挑（见 handleLoginTest 的 switch），填了 token 就再也不测密码，用户以为
   在测密码、看到的却是 token 被拒。
   框里还是脱敏哨兵时发空串：服务端不认识哨兵，空串会让它回落到已保存的那份。 */
function loginTestBody() {
  const pick = function (id) {
    const v = getVal(id);
    return maskedLike(v) ? '' : v;
  };
  return { username: pick('lg-username'), password: pick('lg-password') };
}

/* token 框里是哨兵（或空）时发空体：服务端会拿它自己那份按 token → Cookie →
   账密走一遍，回答的正是「今晚到底会不会签上」。粘了新 token 就只测新的那个。 */
function tokenTestBody() {
  const v = getVal('cfg-token');
  return (maskedLike(v) || !v) ? {} : { token: v };
}

function loginTestStatus(text, kind) {
  const el = $('login-test-status');
  if (!el) return;
  el.textContent = str(text);
  el.className = 'hint login-test-line' + (kind ? ' ' + kind : '');
}

/* 配置页那行的状态（token 验证结果落在这里，弹窗收着也看得见） */
function tokenStatus(text, kind) {
  const el = $('token-status');
  if (!el) return;
  el.textContent = str(text);
  el.className = 'hint login-test-line' + (kind ? ' ' + kind : '');
}

function loginModeText(v) {
  const s = str(v);
  return LOGIN_MODE_TEXT[s] || s || '未知方式';
}

/* 成功回包一行长度有限，hops 只报步数，details 也不整段铺开 */
function loginTestResultText(r) {
  const parts = ['登录成功（' + loginModeText(r && r.mode) + '）'];
  if (r && str(r.message)) parts.push(str(r.message));
  if (r && str(r.token_hint)) parts.push('免登凭证 ' + str(r.token_hint));
  if (r && r.school_ok === true) parts.push('签到接口可访问');
  else if (r && r.school_ok === false) parts.push('签到接口暂时不可访问');
  if (r && str(r.plan)) parts.push('今日计划 ' + str(r.plan));
  const hops = (r && Array.isArray(r.hops)) ? r.hops.length : 0;
  if (hops) parts.push('走完 ' + hops + ' 步');
  return parts.join(' · ');
}

function loginTestErrorText(e) {
  let text = (e && e.message) ? str(e.message) : '测试登录失败';
  const details = (e && Array.isArray(e.details)) ? e.details.map(str).filter(function (x) { return x.trim() !== ''; }) : [];
  /* 逐跳诊断每步一行：挤成一行会把 GET 请求地址里的 service 参数这种关键信息截掉，
     而这正是用户唯一能用来判断"断在哪一步"的东西 */
  if (details.length) text += '\n' + details.join('\n');
  return text.length > 600 ? text.slice(0, 600) + '…' : text;
}

/* ---------- 6.1.1 校外 IP 的短信二次验证（409 second_auth） ---------- */

function isSecondAuth(e) {
  const p = e && e.payload;
  return !!(p && (p.second_auth || str(p.reason) === 'second_auth'));
}

/* 新一轮「测试登录 / 获取凭证」开始：指引块与短信入口先收起
   （面板正开着就留着：那是另一条正在走的路，不该被顺手关掉） */
function resetSecondAuthHints() {
  setHidden('login-second-auth', true);
  if (isHidden('sms-auth')) setHidden('btn-sms-start', true);
}

/* 409 分两种：带 challenge = 后端已经把码发出去了，直接开面板等 6 位数字；
   不带 challenge = 这次发不出去（只有 Cookie、或学校那边拒了），留指引块 + 手动入口兜底。 */
function showSecondAuth(e) {
  const p = (e && e.payload) || {};
  const sa = p.second_auth || {};
  /* 弹窗是这条路的唯一容器：从「验证这份凭证」撞上 409 时它可能还收着 */
  openLoginModal();
  const head = str(p.message) || str(e && e.message) || '学校要求短信二次验证。';
  const tip = str(sa.tip);
  const ticket = str(p.challenge);
  /* 学校那句原话只在"没有面板"时才贴在状态行里：面板自己会显示 tip，
     同一句话在上面和框里各出现一次只会让人以为报了两遍错。 */
  loginTestStatus([head, ticket ? '' : tip].filter(Boolean).join('\n'), ticket ? '' : 'err');
  if (ticket) {
    setHidden('login-second-auth', true);
    smsOpen({
      challenge: ticket,
      phone: str(sa.phone),
      tip: tip,
      sent: p.sent !== false,
      expires_in: p.expires_in,
      resend_after: p.resend_after
    });
    return;
  }
  setHidden('login-second-auth', false);
  const why = str(p.sms_error) || '这次没能自动发验证码：先在弹窗里填上「学号」「密码」，再点「发一条短信验证码」。';
  if (isHidden('sms-auth')) {
    setHidden('btn-sms-start', false);
    smsStatus(why, 'err');
    return;
  }
  /* 面板正开着（上一次的码可能还在路上）：别用这句盖掉「已发到某某手机」那行，
     否则同一个框里会同时出现"码已发出"和"没能发码"两句互相打架的话。 */
  setHidden('btn-sms-start', false);
  loginTestStatus(why, 'err');
}

async function testLogin(btn) {
  const btnEl = btn || $('btn-login-test');
  resetSecondAuthHints();
  setBusy(btnEl, '测试中…');
  loginTestStatus('正在用这组学号密码走一遍学校登录…', '');
  try {
    /* quiet：结论就在下面这一行显示，别再叠一个 toast；
       keepSession：登录失败可能回 502/401，那是「这条凭据不通」，不是管理端会话失效。 */
    const r = await apiFetch('/login/test', { method: 'POST', body: loginTestBody(), keepSession: true, quiet: true });
    loginTestStatus(loginTestResultText(r), 'ok');
    announce('测试登录成功');
  } catch (e) {
    if (isSecondAuth(e)) showSecondAuth(e);
    else loginTestStatus(loginTestErrorText(e), 'err');
  } finally {
    clearBusy(btnEl);
  }
}

/* 配置页的「验证这份凭证」：只测 token（或服务端手上那套），不碰学号密码 */
async function testToken(btn) {
  const btnEl = btn || $('btn-token-test');
  const body = tokenTestBody();
  const saved = !body.token;
  tokenStatus(saved ? '正在验证服务端已保存的凭据…' : '正在验证这份免登凭证…', '');
  setBusy(btnEl, '验证中…');
  try {
    const r = await apiFetch('/login/test', { method: 'POST', body: body, keepSession: true, quiet: true });
    const text = loginTestResultText(r);
    tokenStatus(saved ? '已保存的凭据可用：' + text : text, 'ok');
    announce('凭证验证通过');
  } catch (e) {
    if (isSecondAuth(e)) {
      /* 这条路上没有账密可发码，弹窗里会说明原因并给出「发一条短信验证码」 */
      showSecondAuth(e);
      tokenStatus('学校要短信验证：在弹窗里用学号密码登录一次', 'err');
    } else {
      tokenStatus(loginTestErrorText(e), 'err');
    }
  } finally {
    clearBusy(btnEl);
  }
}

/* ---------- 6.1 一键换新免登凭证 ---------- */

/* 与「测试登录」的区别只有一条：这个会写配置。
   换到的凭证先由服务端拿去做一次 init 校验，通过了才落盘 —— 所以按钮点完
   不需要再点「保存」，页面上那个「已配置」徽标也会跟着刷新。 */
async function refreshToken(btn) {
  const btnEl = btn || $('btn-login-refresh');
  resetSecondAuthHints();
  setBusy(btnEl, '获取中…');
  loginTestStatus('正在用当前凭据换新免登凭证…', '');
  try {
    const r = await apiFetch('/login/refresh', { method: 'POST', body: loginTestBody(), keepSession: true, quiet: true });
    const parts = ['已获取免登凭证 ' + str(r.token_hint) + '，已写入配置'];
    if (str(r.plan)) parts.push(str(r.plan));
    loginTestStatus(parts.join(' · '), 'ok');
    announce('已获取免登凭证');
    /* 重拉配置：token 框与「已配置」徽标要跟着变；弹窗里刚打的账密要留住 */
    await reloadConfigKeepAccount();
  } catch (e) {
    if (isSecondAuth(e)) showSecondAuth(e);
    else loginTestStatus(loginTestErrorText(e), 'err');
  } finally {
    clearBusy(btnEl);
  }
}

/* ---------- 6.1.2 短信验证码登录（机房 IP 上密码过了还得补一条六位码） ---------- */
/* 服务端给的那串 32 位只是「这次验证」的门牌号，不是凭证：面板一收就作废。 */

const SMS_CODE_LEN = 6;
/* 凑满 6 位 → 等这么久再自动提交：留给「还在粘贴 / 连打最后一位」的收手时间 */
const SMS_DEBOUNCE_MS = 250;

let smsTicket = '';
let smsTickTimer = 0;
let smsTickUntil = 0;
let smsBusy = false;      // 单飞：同一时刻只允许一个 verify 在飞
let smsTimer = 0;         // 自动提交的防抖计时器

function smsStatus(text, kind) {
  const el = $('sms-status');
  if (!el) return;
  el.textContent = str(text);
  el.className = 'sms-status' + (kind ? ' ' + kind : '');
}

/* 忙态不走 setBusy：它会换掉整个子节点，和倒计时抢文案 */
function smsBtn(id, text, busy) {
  const btn = $(id);
  if (!btn) return;
  if (busy) btn.setAttribute('aria-busy', 'true');
  else btn.removeAttribute('aria-busy');
  btn.disabled = !!busy;
  if (text !== undefined && text !== null) btn.textContent = str(text);
}

/* 发送键的唯一出口：倒计时没走完就锁住 */
function smsSendPaint() {
  const btn = $('btn-sms-send');
  if (!btn) return;
  const left = smsTickUntil ? Math.ceil((smsTickUntil - Date.now()) / 1000) : 0;
  if (left > 0) {
    btn.disabled = true;
    btn.textContent = '重发(' + left + 's)';
    return;
  }
  if (smsTickTimer) { clearInterval(smsTickTimer); smsTickTimer = 0; }
  smsTickUntil = 0;
  btn.disabled = false;
  btn.textContent = smsTicket ? '重新发送' : '发送验证码';
}

/* 后端给多少秒就倒多少秒（429 同理） */
function smsCountdown(seconds) {
  if (smsTickTimer) { clearInterval(smsTickTimer); smsTickTimer = 0; }
  const secs = Math.max(0, Math.round(Number(seconds) || 0));
  smsTickUntil = secs > 0 ? Date.now() + secs * 1000 : 0;
  smsSendPaint();
  if (smsTickUntil) smsTickTimer = setInterval(smsSendPaint, 500);
}

/* 从后端那句话里取秒数，取不到按 30 秒 */
function smsSeconds(msg) {
  const m = /(\d+)/.exec(str(msg));
  const n = m ? Number(m[1]) : 0;
  return (n > 0) ? Math.min(3600, n) : 30;
}

/* 只显示服务端打码过的号，前端不还原 */
function smsPhoneText(v) { return str(v).trim() || '你的手机'; }

function smsIsOpen() { return !isHidden('sms-auth'); }

/* 框里只认数字：不管手输还是粘贴，取出来一律先洗一遍 */
function smsCode() { return getVal('sms-code').replace(/[^0-9]/g, ''); }

/* 校验中把输入框置为只读：这时再敲键盘也不该攒出第二个请求 */
function smsEdit(on) {
  const el = $('sms-code');
  if (!el) return;
  el.readOnly = !!on;
  if (on) el.setAttribute('aria-busy', 'true');
  else el.removeAttribute('aria-busy');
}

/* 凑满 6 位才排自动提交；提交在飞/面板已收/没有门牌号都不排 */
function smsSchedule() {
  if (smsTimer) { clearTimeout(smsTimer); smsTimer = 0; }
  if (!smsIsOpen() || !smsTicket || smsBusy) return;
  if (smsCode().length !== SMS_CODE_LEN) return;
  smsTimer = setTimeout(function () { smsTimer = 0; smsVerify(); }, SMS_DEBOUNCE_MS);
}

function smsFocusCode() {
  const el = $('sms-code');
  if (el) setTimeout(function () { el.focus(); }, 30);
}

function smsOpen(r) {
  smsTicket = str(r && r.challenge);
  if (smsTimer) { clearTimeout(smsTimer); smsTimer = 0; }
  setHidden('btn-sms-start', true);
  setHidden('sms-auth', false);
  setText('sms-phone', smsPhoneText(r && r.phone));
  const tip = str(r && r.tip);
  const tipEl = $('sms-tip');
  if (tipEl) { tipEl.textContent = tip; tipEl.hidden = !tip; }
  setVal('sms-code', '');
  smsEdit(false);
  const mins = Math.max(1, Math.round((Number(r && r.expires_in) || 300) / 60));
  /* reused=true 表示服务端复用了还活着的那条验证码（没有重新发）。这时候说"已发到"
     就是假话，直接把后端那句原话显示出来。 */
  const msg = str(r && r.message);
  if (msg) smsStatus(msg, 'ok');
  else if (r && r.sent === false) smsStatus('上一条验证码还在有效期内，直接填 6 位数字就行（' + mins + ' 分钟内有效）', 'ok');
  else smsStatus('验证码已发到 ' + smsPhoneText(r && r.phone) + '，直接填 6 位数字就行（' + mins + ' 分钟内有效）', 'ok');
  smsCountdown(r && r.resend_after);
  smsFocusCode();
}

/* 收起面板：验证、已填的码、倒计时、排队中的自动提交一起清掉 */
function smsCollapse() {
  setHidden('sms-auth', true);
  smsTicket = '';
  if (smsTickTimer) { clearInterval(smsTickTimer); smsTickTimer = 0; }
  if (smsTimer) { clearTimeout(smsTimer); smsTimer = 0; }
  smsTickUntil = 0;
  setVal('sms-code', '');
  setHidden('sms-tip', true);
  setText('sms-phone', '');
  smsStatus('');
  smsSendPaint();
}

/* 过期：清掉门牌号与已填的码，发送键回初始态 */
function smsExpire(msg) {
  smsTicket = '';
  if (smsTickTimer) { clearInterval(smsTickTimer); smsTickTimer = 0; }
  if (smsTimer) { clearTimeout(smsTimer); smsTimer = 0; }
  smsTickUntil = 0;
  setVal('sms-code', '');
  smsSendPaint();
  smsStatus((str(msg) || '这次验证过期了') + '。重新点「发送验证码」再来一条。', 'err');
  smsFocusCode();
}

async function smsStart(btn) {
  const fromSend = !!(btn && btn.id === 'btn-sms-send');
  const target = fromSend ? $('btn-sms-send') : (btn || $('btn-sms-start'));
  if (target) smsBtn(target.id, '发送中…', true);
  loginTestStatus('正在请学校发一条短信验证码…', '');
  try {
    /* 带上弹窗里这组账密：空串等于"没填"，服务端会回落到已保存的那份 */
    const r = await apiFetch('/login/sms/start', {
      method: 'POST', body: loginTestBody(), keepSession: true, quiet: true, localRate: true
    });
    if (!str(r && r.challenge)) {
      /* 校内网络：不用验证码，这一步就登录成功 */
      smsCollapse();
      setHidden('btn-sms-start', true);
      setHidden('login-second-auth', true);
      const ok = str(r && r.message) || '这台机器的网络不用短信验证，已经登录成功。';
      loginTestStatus(ok, 'ok');
      smsStatus(ok, 'ok');
      announce('登录成功');
      reloadConfigKeepAccount();
      loadDashboard(false);
      return;
    }
    smsOpen(r);
    loginTestStatus('学校要一条短信验证码，填在下面这一行里。', '');
  } catch (e) {
    const p = (e && e.payload) || {};
    const reason = str(p.reason);
    const msg = str(p.message);
    if (reason === 'no_credentials') smsStatus(msg || '先填「学号」「密码」：学校才肯把码发到登记的手机。', 'err');
    else if (reason === 'phone_unbound') smsStatus(msg || '这个学号没绑手机号，短信发不出来；改用免登凭证。', 'err');
    else if (isSecondAuth(e)) showSecondAuth(e);
    else smsStatus(msg || loginTestErrorText(e), 'err');
    if (isHidden('sms-auth')) setHidden('btn-sms-start', false);
  } finally {
    if (fromSend) smsSendPaint();
    else smsBtn('btn-sms-start', '发一条短信验证码', false);
  }
}

async function smsResend(btn) {
  smsBtn('btn-sms-send', '发送中…', true);
  smsStatus('正在请学校再发一条…', '');
  try {
    const r = await apiFetch('/login/sms/resend', {
      method: 'POST', body: { challenge: smsTicket }, keepSession: true, quiet: true, localRate: true
    });
    smsCountdown(r && r.resend_after);
    setVal('sms-code', '');
    const mins = Math.max(1, Math.round((Number(r && r.expires_in) || 300) / 60));
    smsStatus('新的验证码已经发出，旧的那条作废了；' + mins + ' 分钟内有效。', 'ok');
    smsFocusCode();
  } catch (e) {
    const p = (e && e.payload) || {};
    const msg = str(p.message);
    if (e.status === 429) {
      smsCountdown(smsSeconds(msg));
      smsStatus(msg || '刚发过一条，等倒计时走完再重发。', 'err');
    } else if (e.status === 410 || str(p.reason) === 'challenge_expired') {
      smsExpire(msg);
    } else {
      smsStatus(msg || loginTestErrorText(e), 'err');
    }
  } finally {
    smsSendPaint();
  }
}

/* 自动提交与「提交验证码」按钮/回车共用这一条路：单飞 + 失败不自动重试。
   用户从 409 进来时手机已经收到码了，凑满 6 位就直接送，不再让人找按钮。 */
async function smsVerify() {
  if (smsTimer) { clearTimeout(smsTimer); smsTimer = 0; }
  const codeInput = $('sms-code');
  const code = smsCode();
  if (!smsTicket) {
    smsStatus('这次验证失效了，点「发送验证码」重来一条。', 'err');
    return;
  }
  if (code.length !== SMS_CODE_LEN) {
    smsStatus('验证码是短信里那 6 位数字，现在只有 ' + code.length + ' 位。', 'err');
    if (codeInput) codeInput.focus();
    return;
  }
  /* 单飞：已经有一个在飞了，这串数字的结论马上会回来，不再叠第二个请求 */
  if (smsBusy) return;
  smsBusy = true;
  smsBtn('btn-sms-verify', '提交中…', true);
  smsEdit(true);
  smsStatus('校验中…', '');
  try {
    const r = await apiFetch('/login/sms/verify', {
      method: 'POST',
      body: { challenge: smsTicket, code: code },
      keepSession: true, quiet: true, localRate: true
    });
    const parts = ['短信验证通过，已经登录成功'];
    if (str(r && r.token_hint)) parts.push('免登凭证 ' + str(r && r.token_hint));
    if (r && r.school_ok === true) parts.push('签到系统已连通');
    else if (r && r.school_ok === false) parts.push('签到系统暂时连不上');
    if (str(r && r.plan)) parts.push('今日计划 ' + str(r && r.plan));
    if (str(r && r.message)) parts.push(str(r && r.message));
    const done = parts.join(' · ');
    smsCollapse();
    setHidden('btn-sms-start', true);
    setHidden('login-second-auth', true);
    /* 弹窗还开着：这句就是「这次登录的结论」，关掉才看不见 */
    smsStatus(done, 'ok');
    loginTestStatus('短信验证通过，已经登录成功；免登凭证已写进配置。', 'ok');
    announce('已用短信验证码登录');
    /* 服务端只写 token，弹窗里刚打的账密不能被刷回旧值 */
    reloadConfigKeepAccount();
    loadDashboard(false);
  } catch (e) {
    const p = (e && e.payload) || {};
    const msg = str(p.message);
    const reason = str(p.reason);
    if (e.status === 400 && reason === 'code_rejected') {
      /* 清空 + 聚焦，让用户直接重打：不自动重试（重试只会把 attempts_left 烧光） */
      const left = Number(p.attempts_left);
      const tail = Number.isFinite(left) ? '，还能再试 ' + left + ' 次' : '';
      smsStatus((msg || '验证码不对或已过期') + tail + '。', 'err');
      setVal('sms-code', '');
      if (codeInput && smsIsOpen()) codeInput.focus();
    } else if (e.status === 410 || reason === 'challenge_expired') {
      smsExpire(msg);
    } else if (e.status === 429) {
      smsCountdown(smsSeconds(msg));
      smsStatus(msg || '试得太频繁了，等倒计时走完再填。', 'err');
    } else {
      smsStatus(msg || loginTestErrorText(e), 'err');
    }
  } finally {
    smsBusy = false;
    smsEdit(false);
    smsBtn('btn-sms-verify', '提交验证码', false);
  }
}

function smsCancel() {
  const ticket = smsTicket;
  smsCollapse();
  setHidden('btn-sms-start', false);
  loginTestStatus('已取消这次短信验证', '');
  if (ticket) {
    /* 取消不掉也无所谓：那条 5 分钟后自己过期 */
    apiFetch('/login/sms/cancel', {
      method: 'POST', body: { challenge: ticket }, keepSession: true, quiet: true, localRate: true
    }).catch(function () {});
  }
}

/* ---------- 6.2 浏览器定位：WGS-84 → 高德 GCJ-02 ---------- */

/* GCJ-02 偏移公式的椭球参数（克拉索夫斯基 1940），标准实现的固定值 */
const GCJ_A = 6378245.0;
const GCJ_EE = 0.00669342162296594323;
const GCJ_PI = 3.1415926535897932384626;

/* 模块级 busy：定位期间再点只会再挂一个被浏览器忽略的请求，直接挡住更省事 */
let geoBusy = false;

/* 境外不偏移：GCJ-02 只对国内坐标加偏移，对境外点套公式会把它推走几百米，必须原样返回 */
function outOfChina(lng, lat) {
  return !(lng > 73.66 && lng < 135.05 && lat > 3.86 && lat < 53.55);
}

function transformLat(x, y) {
  let ret = -100.0 + 2.0 * x + 3.0 * y + 0.2 * y * y + 0.1 * x * y + 0.2 * Math.sqrt(Math.abs(x));
  ret += (20.0 * Math.sin(6.0 * x * GCJ_PI) + 20.0 * Math.sin(2.0 * x * GCJ_PI)) * 2.0 / 3.0;
  ret += (20.0 * Math.sin(y * GCJ_PI) + 40.0 * Math.sin(y / 3.0 * GCJ_PI)) * 2.0 / 3.0;
  ret += (160.0 * Math.sin(y / 12.0 * GCJ_PI) + 320 * Math.sin(y * GCJ_PI / 30.0)) * 2.0 / 3.0;
  return ret;
}

function transformLng(x, y) {
  let ret = 300.0 + x + 2.0 * y + 0.1 * x * x + 0.1 * x * y + 0.1 * Math.sqrt(Math.abs(x));
  ret += (20.0 * Math.sin(6.0 * x * GCJ_PI) + 20.0 * Math.sin(2.0 * x * GCJ_PI)) * 2.0 / 3.0;
  ret += (20.0 * Math.sin(x * GCJ_PI) + 40.0 * Math.sin(x / 3.0 * GCJ_PI)) * 2.0 / 3.0;
  ret += (150.0 * Math.sin(x / 12.0 * GCJ_PI) + 300.0 * Math.sin(x / 30.0 * GCJ_PI)) * 2.0 / 3.0;
  return ret;
}

/* 浏览器 Geolocation 给的是 WGS-84（GPS 原始坐标），服务端判签到范围用的是高德 GCJ-02。
   少这一步在福州会偏出约 590 米，结论直接变成「不在签到范围」。
   参考向量：WGS-84 119.20463,26.04920 → GCJ-02 119.209433,26.046112 */
function wgs84ToGcj02(lng, lat) {
  if (outOfChina(lng, lat)) return { lng: lng, lat: lat };
  let dLat = transformLat(lng - 105.0, lat - 35.0);
  let dLng = transformLng(lng - 105.0, lat - 35.0);
  const radLat = lat / 180.0 * GCJ_PI;
  let magic = Math.sin(radLat);
  magic = 1 - GCJ_EE * magic * magic;
  const sqrtMagic = Math.sqrt(magic);
  dLat = (dLat * 180.0) / ((GCJ_A * (1 - GCJ_EE)) / (magic * sqrtMagic) * GCJ_PI);
  dLng = (dLng * 180.0) / (GCJ_A / sqrtMagic * Math.cos(radLat) * GCJ_PI);
  return { lng: lng + dLng, lat: lat + dLat };
}

/* 状态行：类名 ok / err / busy 一律由这里先清空再按需挂上，避免成功、失败、
   进行中三条路径各写一遍。'busy' 必须一起删——它是唯一驱动左侧转圈动画的类
   （style.css 的 .geo-status.busy::before），只在调用方传 'busy' 时加回来；
   少删一次就会在定位成功后一直转下去。 */
function geoStatus(text, kind) {
  const el = $('geo-status');
  if (!el) return;
  el.textContent = str(text);
  el.classList.remove('ok', 'err', 'busy');
  if (kind) el.classList.add(kind);
}

/* 直接改 value 不会触发配置表单的 input 监听（徽标/提示靠它同步），补发一次冒泡 input */
function fillField(el, v) {
  if (!el) return;
  el.value = str(v);
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

function fillCoord(id, v) { fillField($(id), v); }

function onGeoOk(pos) {
  const c = (pos && pos.coords) || {};
  const g = wgs84ToGcj02(Number(c.longitude), Number(c.latitude));
  if (!Number.isFinite(g.lng) || !Number.isFinite(g.lat)) {
    onGeoErr({ code: 0, message: '浏览器返回的坐标不可用' });
    return;
  }
  /* 保留 6 位小数：约 0.1 米，远小于 GPS 误差，也让回填值和配置里手填的格式一致 */
  fillCoord('cfg-lon', g.lng.toFixed(6));
  fillCoord('cfg-lat', g.lat.toFixed(6));
  const acc = Math.round(Number(c.accuracy) || 0);
  let msg = '已定位：精度约 ±' + acc + ' 米，已转换为高德 GCJ-02，请确认是否为你的常驻位置。';
  if (acc > 100) msg += ' 精度较低（可能在室内），建议到窗边重试。';
  geoStatus(msg, 'ok');
  announce('已定位并填入 GCJ-02 坐标（未保存）');
}

function onGeoErr(e) {
  const code = Number(e && e.code);
  let msg;
  if (code === 1) msg = '浏览器拒绝了定位权限，请在地址栏左侧的站点设置里允许位置，然后重试';
  else if (code === 2) msg = '系统定位服务不可用（GPS/Wi-Fi 定位被关闭？）';
  else if (code === 3) msg = '定位超时，请重试或手动填写坐标';
  else msg = '定位失败：' + (str(e && e.message) || '未知错误');
  geoStatus(msg, 'err');
  toast('error', msg);
}

/* 点击主流程：只回填经纬度，不碰「上报地址文案」，也不自动保存（要用户自己确认后点保存） */
function locateNow(btn) {
  if (geoBusy) return;
  if (!navigator.geolocation) {
    geoStatus('当前浏览器不支持定位', 'err');
    toast('error', '当前浏览器不支持定位');
    return;
  }
  /* 非安全上下文（局域网 IP + http 就是本服务最常见的形态）下浏览器会直接以
     PERMISSION_DENIED 拒绝，错误码看着像「用户点了拒绝」，所以必须单独拦下给可执行提示 */
  if (window.isSecureContext === false) {
    geoStatus('浏览器只在 HTTPS 或 localhost/127.0.0.1 下允许定位；用局域网 IP 以 http 打开时需要改用 127.0.0.1 或配 HTTPS', 'err');
    toast('error', '定位需要安全上下文：改用 http://127.0.0.1 或 HTTPS 访问');
    return;
  }
  geoBusy = true;
  setBusy(btn, '定位中…');
  // 用 'busy' 点亮状态行的自绘转圈点（ui 侧已实现 .geo-status.busy），
  // 比一行静止文字更能说明"正在等你点允许"。
  geoStatus('申请定位权限…', 'busy');
  const finish = function (err, pos) {
    geoBusy = false;
    clearBusy(btn);
    if (err) onGeoErr(err); else onGeoOk(pos);
  };
  try {
    navigator.geolocation.getCurrentPosition(
      function (pos) { finish(null, pos); },
      function (e) { finish(e, null); },
      { enableHighAccuracy: true, timeout: 15000, maximumAge: 0 }
    );
  } catch (e) {
    finish({ code: 0, message: e && e.message }, null);
  }
}

/* ---------- 6.3 地图选点：懒加载 map-picker.js ---------- */

/* 选点器自己实现了瓦片拼接、拖动缩放和坐标系换算，有 10 KB 出头；塞进 app.js 会顶到
   体积预算（80 KiB）——所以走动态 import：不点「地图选点」就一个字节都不下载。
   CSP 的 script-src 'self' 对同源 module 的动态导入一样放行，不需要放宽脚本策略。 */
let pickerPromise = null;

function loadPicker() {
  if (!pickerPromise) {
    pickerPromise = import('./map-picker.js').catch(function (e) {
      pickerPromise = null; // 失败要能重试：一次网络抖动不该让按钮永久失效
      throw e;
    });
  }
  return pickerPromise;
}

/* row 为 null → 填上面那组单个坐标；给了行 → 只改位置池里这一行。
   两处都要发 input 事件：位置池提示、已配置徽标都挂在 form-config 的 input 监听上。 */
async function openMapPicker(row) {
  let mod;
  try {
    mod = await loadPicker();
  } catch (e) {
    toast('error', '选点模块没加载起来：刷新页面后再点一次');
    return;
  }
  const lonEl = row ? row.querySelector('.r-lon') : null;
  const latEl = row ? row.querySelector('.r-lat') : null;
  const actEl = row ? row.querySelector('.r-actual') : $('cfg-actual');
  try {
    mod.openPicker({
      lng: row ? (lonEl ? lonEl.value.trim() : '') : getVal('cfg-lon'),
      lat: row ? (latEl ? latEl.value.trim() : '') : getVal('cfg-lat'),
      /* 把现有的「上报地点文字」带进去：空的话由选点器去取/带出记忆，非空就原样摆出来 */
      name: actEl ? String(actEl.value || '').trim() : '',
      toGcj: wgs84ToGcj02,
      onConfirm: function (p) {
        const lon = Number(p && p.lng);
        const lat = Number(p && p.lat);
        if (!Number.isFinite(lon) || !Number.isFinite(lat)) return;
        /* 6 位小数 ≈ 0.1 米，比 GPS 误差小得多，也跟手填的格式对齐 */
        const lonTxt = lon.toFixed(6);
        const latTxt = lat.toFixed(6);
        if (row) {
          fillField(lonEl, lonTxt);
          fillField(latEl, latTxt);
        } else {
          fillCoord('cfg-lon', lonTxt);
          fillCoord('cfg-lat', latTxt);
        }
        /* 地点文字跟坐标一起同步：选点器里确认过的那串就是标准答案，空着就不动原值 */
        const name = String((p && p.name) || '').trim();
        if (name) fillField(actEl, name);
        const tail = name ? '，地点文字已同步' : '（地点文字没取到，可手动补）';
        geoStatus('已在地图上选点：' + lonTxt + ', ' + latTxt + '（高德 GCJ-02，点「保存」后生效）' + tail, 'ok');
        announce('已填入地图选点坐标与地点文字');
        toast('ok', name ? '坐标与地点文字已填入，记得点「保存」' : '坐标已填入，地点文字可再补一句');
      }
    });
  } catch (e) {
    toast('error', '地图选点打不开：' + (e && e.message ? e.message : '未知错误'));
  }
}

/* ---------- 7. 导入 / 导出 ---------- */

function openImport() {
  showOverlay('modal-import');
  clearBox('import-err');
  showWarnings('import-warnings', []);
  const ul = $('import-preview');
  if (ul) ul.textContent = '';
  const apply = $('btn-import-apply');
  if (apply) apply.disabled = true;
  S.importPreview = null;
  const ta = $('import-yaml');
  if (ta) setTimeout(function () { ta.focus(); }, 30);
}

function closeImport() {
  setHidden('modal-import', true);
}

function previewLine(ul, text) {
  const li = document.createElement('li');
  li.textContent = text;
  ul.append(li);
}

function renderImportPreview(cfg, warnings) {
  const ul = $('import-preview');
  if (!ul) return;
  ul.textContent = '';
  if (!cfg) return;
  const user = cfg.user || {};
  const ck = cfg.checkin || {};
  const sch = ck.schedule || {};
  const nt = cfg.notify || {};
  const vac = cfg.vacation || {};
  const srv = cfg.server || {};
  const times = Array.isArray(sch.times) ? sch.times : [];
  const locs = Array.isArray(ck.locations) ? ck.locations : [];
  const ranges = Array.isArray(vac.skip_ranges) ? vac.skip_ranges : [];

  previewLine(ul, '签到窗口：' + (str(sch.window_start) || '默认') + ' – ' + (str(sch.window_end) || '默认'));
  previewLine(ul, '签到时刻：' + (times.length ? times.join(' / ') : '默认 21:35'));
  previewLine(ul, '位置：' + (locs.length ? ('位置池 ' + locs.length + ' 个点') : ((str(ck.longitude) || '?') + ', ' + (str(ck.latitude) || '?'))));
  previewLine(ul, '位置抖动半径：' + (numStr(ck.location_jitter_m) || '默认 200') + ' 米 · 时刻浮动 ±' + (numStr(sch.time_jitter_minutes) || '0') + ' 分钟');
  previewLine(ul, '早到最多等多久 / 晚多少分钟还算准点：' + (numStr(sch.max_wait_minutes) || '12') + ' / ' + (numStr(sch.tolerance_minutes) || '5') + ' 分钟');
  previewLine(ul, '推送：' + notifyName(nt.type));
  previewLine(ul, '假期：' + (ranges.length
    ? ranges.length + ' 段 · ' + ranges.map(function (r) { return (str(r.name) || '未命名') + ' ' + (str(r.start) || '?') + '~' + (str(r.end) || '?'); }).join('；')
    : '无'));
  previewLine(ul, '凭据：Cookie ' + (user.cookie ? '已填' : '空') + ' · 免登凭证 ' + (user.token ? '已填' : '空') + ' · 学号 ' + (user.username ? '已填' : '空') + ' · 密码 ' + (user.password ? '已填' : '空'));
  previewLine(ul, '服务：访问地址 ' + (str(srv.listen) || '默认') + ' · 数据存放目录 ' + (str(srv.data_dir) || '默认'));
  showWarnings('import-warnings', warnings);
}

async function previewImport() {
  const ta = $('import-yaml');
  const text = ta ? ta.value : '';
  clearBox('import-err');
  const ul = $('import-preview');
  if (ul) ul.textContent = '';
  const apply = $('btn-import-apply');
  if (!text.trim()) {
    showError('import-err', { message: '先把 YAML 全文粘进来' });
    return;
  }
  const btn = $('btn-import-preview');
  setBusy(btn, '解析中…');
  try {
    const r = await apiFetch('/config/import-yaml', { method: 'POST', body: { yaml: text } });
    S.importPreview = (r && r.config) ? r.config : null;
    renderImportPreview(S.importPreview, (r && r.warnings) || []);
    if (apply) apply.disabled = !S.importPreview;
  } catch (e) {
    S.importPreview = null;
    if (apply) apply.disabled = true;
    showError('import-err', e, '常见原因：锚点/别名、多文档、块标量不受支持；错误信息会指出第几行。');
  } finally {
    clearBusy(btn);
  }
}

async function applyImport() {
  if (!S.importPreview) return;
  const btn = $('btn-import-apply');
  clearBox('import-err');
  const body = Object.assign({}, S.importPreview);
  setBusy(btn, '保存中…');
  try {
    const r = await apiFetch('/config', { method: 'PUT', body: { config: body } });
    toast('ok', '已应用导入的配置');
    announce('已应用导入的配置');
    closeImport();
    await loadConfig(true);
    if (r && Array.isArray(r.warnings) && r.warnings.length) showWarnings('config-warnings', r.warnings);
    loadDashboard(true);
  } catch (e) {
    showError('import-err', e, '应用失败：按提示修正 YAML 后重新预览。');
  } finally {
    clearBusy(btn);
  }
}

function exportConfig() {
  const a = document.createElement('a');
  a.href = API + '/config/export?format=yaml';
  a.download = 'config.yaml';
  a.rel = 'noopener';
  document.body.append(a);
  a.click();
  a.remove();
  toast('info', '已请求导出（导出的是脱敏版，凭据显示为 ••••）');
}

/* ---------- 8. 日志（SSE） ---------- */

function setConn(state) {
  const el = $('logs-conn');
  if (!el) return;
  el.textContent = CONN_TEXT[state] || state;
  const kind = state === 'open' ? 'ok' : (state === 'connecting' || state === 'reconnecting' || state === 'closed') ? 'warn' : '';
  el.className = 'chip' + (kind ? ' ' + kind : '');
}

function logNode(e) {
  const level = String((e && e.level) || 'info').toLowerCase();
  const li = document.createElement('li');
  li.className = 'log lv-' + (LEVELS[level] !== undefined ? level : 'info');
  const ts = document.createElement('span');
  ts.className = 'ln-ts';
  ts.textContent = fmtTime(e && e.ts);
  const lv = document.createElement('span');
  lv.className = 'ln-lv';
  lv.textContent = level.toUpperCase();
  const msg = document.createElement('span');
  msg.className = 'ln-msg';
  msg.textContent = str(e && e.msg);
  li.append(ts, lv, msg);
  if (e && e.run_id) li.title = '运行编号 ' + str(e.run_id);
  return li;
}

function registerLog(e) {
  if (!e) return false;
  const key = [str(e.ts), str(e.level), str(e.run_id), str(e.msg)].join('\u0001');
  if (S.logKeys.has(key)) return false;
  S.logKeys.add(key);
  S.logs.push(e);
  while (S.logs.length > LOG_CAP) {
    const old = S.logs.shift();
    S.logKeys.delete([str(old.ts), str(old.level), str(old.run_id), str(old.msg)].join('\u0001'));
  }
  return true;
}

function setLogsEmpty() {
  const list = $('logs-list');
  setHidden('logs-empty', !!(list && list.children.length > 0));
}

function appendSnapshot(arr) {
  const list = $('logs-list');
  if (!list) return;
  const frag = document.createDocumentFragment();
  arr.forEach(function (e) {
    if (registerLog(e)) frag.append(logNode(e));
  });
  list.insertBefore(frag, list.firstChild);
  while (list.children.length > LOG_CAP) list.lastElementChild.remove();
  if (S.autoScroll) list.scrollTop = list.scrollHeight;
  setLogsEmpty();
}

function addLog(e) {
  const list = $('logs-list');
  if (!list || !registerLog(e)) return;
  list.append(logNode(e));
  while (list.children.length > LOG_CAP) list.firstElementChild.remove();
  if (S.autoScroll) list.scrollTop = list.scrollHeight;
  setLogsEmpty();
}

function startLogs() {
  if (!S.logsSnapshotted) {
    S.logsSnapshotted = true;
    apiFetch('/logs?limit=' + LOG_INITIAL, { quiet: true }).then(function (d) {
      const arr = (d && Array.isArray(d.logs)) ? d.logs.slice().sort(function (a, b) {
        return (toMs(a && a.ts) || 0) - (toMs(b && b.ts) || 0);
      }) : [];
      appendSnapshot(arr);
    }).catch(function () { /* 快照失败不阻塞实时流 */ });
  }
  openStream();
}

function openStream() {
  closeStream();
  if (typeof window.EventSource !== 'function') { setConn('unsupported'); return; }
  let src = null;
  try {
    src = new EventSource(API + '/logs/stream');
  } catch (e) {
    setConn('closed');
    scheduleReconnect();
    return;
  }
  S.es = src;
  setConn('connecting');
  src.addEventListener('open', function () {
    S.esRetry = 0;
    setConn('open');
  });
  src.addEventListener('log', function (ev) {
    const d = parseJSON(ev.data);
    if (d) addLog(d);
  });
  src.addEventListener('run', function (ev) {
    const d = parseJSON(ev.data);
    if (!d) return;
    announce('运行结束：' + (STATUS_TEXT[d.status] || str(d.status) || '未知') + (d.id ? '（' + str(d.id) + '）' : ''));
  });
  src.addEventListener('error', function () {
    if (S.es !== src) return;
    if (src.readyState === 2) {
      setConn('closed');
      try { src.close(); } catch (e) { /* ignore */ }
      S.es = null;
      if (S.route === '#/logs') scheduleReconnect();
    } else {
      setConn('reconnecting');
    }
  });
}

function scheduleReconnect() {
  if (S.esTimer) return;
  const wait = Math.min(30000, 3000 * Math.pow(2, S.esRetry));
  S.esRetry = Math.min(S.esRetry + 1, 5);
  S.esTimer = setTimeout(function () {
    S.esTimer = 0;
    if (S.route === '#/logs') openStream();
  }, wait);
}

function closeStream() {
  if (S.esTimer) { clearTimeout(S.esTimer); S.esTimer = 0; }
  if (S.es) {
    try { S.es.close(); } catch (e) { /* ignore */ }
    S.es = null;
  }
  setConn('idle');
}

function clearLogs() {
  const list = $('logs-list');
  if (list) list.textContent = '';
  S.logs = [];
  S.logKeys = new Set();
  setLogsEmpty();
  toast('info', '已清屏（服务端内存环里的日志仍在）');
}

async function copyLogs() {
  const list = $('logs-list');
  if (!list) return;
  const lines = [];
  each(list.children, function (li) {
    if (li.offsetParent === null) return;
    lines.push(li.textContent);
  });
  if (!lines.length) { toast('info', '没有可复制的日志'); return; }
  const text = lines.join('\n');
  try {
    if (!navigator.clipboard || !navigator.clipboard.writeText) throw new Error('no clipboard');
    await navigator.clipboard.writeText(text);
    toast('ok', '已复制 ' + lines.length + ' 行');
  } catch (e) {
    try {
      const range = document.createRange();
      range.selectNodeContents(list);
      const sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      toast('warn', '剪贴板不可用（需要安全上下文），已选中日志，请按 Ctrl+C');
    } catch (e2) {
      toast('error', '复制失败，请手动选中文本');
    }
  }
}

/* ---------- 9. 运行历史 ---------- */

function runsLimit() {
  return Math.min(1000, Math.max(1, S.runsPageSize) * Math.max(1, S.runsPages));
}

async function loadRuns(showToast) {
  const quiet = !showToast;
  const btn = $('btn-runs-more');
  let loaded = false;
  try {
    const d = await apiFetch('/runs?limit=' + runsLimit(), { quiet: quiet });
    S.runs = (d && Array.isArray(d.runs)) ? d.runs : [];
    S.runsExhausted = S.runs.length < runsLimit();
    loaded = true;
  } catch (e) {
    if (e.status !== 401 && !quiet) toast('error', e.message || '读取运行历史失败');
    else if (e.status !== 401) setText('runs-count', '读取失败，稍后重试');
  } finally {
    clearBusy(btn);
  }
  /* renderRuns 会按「还有没有更多」把按钮 disabled 掉；必须排在 clearBusy 之后，
     否则刚设好的 disabled 会被「解除忙碌」重新改成可点（加载到底还能一直点）。 */
  if (loaded) renderRuns();
}

function renderRuns() {
  const list = $('runs-list');
  if (!list) return;
  list.textContent = '';
  S.runs.forEach(function (r) {
    const li = document.createElement('li');
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'run-item';
    btn.dataset.id = str(r && r.id);

    const when = document.createElement('span');
    when.className = 'ri-when';
    when.textContent = fmtDateTime(r && r.started_at);

    const stBox = document.createElement('span');
    const chip = document.createElement('span');
    const kind = STATUS_KIND[r && r.status] || '';
    chip.className = 'chip' + (kind ? ' ' + kind : '');
    chip.textContent = STATUS_TEXT[r && r.status] || str(r && r.status) || '—';
    stBox.append(chip);

    const lat = document.createElement('span');
    lat.className = 'ri-lat';
    lat.textContent = fmtLatency(r && r.latency_ms);

    const tail = document.createElement('span');
    const errText = str(r && r.error);
    tail.className = errText ? 'ri-err' : 'ri-ok';
    tail.textContent = errText || ('决策：' + decisionText(r && r.decision));

    btn.append(when, stBox, lat, tail);
    li.append(btn);
    list.append(li);
  });
  setHidden('runs-empty', S.runs.length > 0);
  setText('runs-count', S.runs.length ? ('已加载 ' + S.runs.length + ' 条 / 请求 limit=' + runsLimit()) : '');
  const more = $('btn-runs-more');
  if (more) more.disabled = S.runs.length === 0 || S.runsExhausted;
}

async function openRun(id) {
  if (!id) return;
  showOverlay('drawer');
  const body = $('drawer-body');
  if (body) body.textContent = '读取中…';
  setText('drawer-title', '运行详情 · ' + id);
  let rec = null;
  try {
    const d = await apiFetch('/runs/' + encodeURIComponent(id), { quiet: true });
    rec = (d && d.run) ? d.run : null;
  } catch (e) {
    rec = null;
  }
  if (!rec) rec = S.runs.filter(function (r) { return r && str(r.id) === id; })[0] || null;
  if (!rec) {
    if (body) body.textContent = '读取失败：这条记录可能已被轮转删除。';
    return;
  }
  renderRunDetail(rec);
  const close = $('drawer-close');
  if (close) close.focus();
}

function closeDrawer() { setHidden('drawer', true); }

function renderRunDetail(r) {
  const body = $('drawer-body');
  if (!body) return;
  body.textContent = '';
  setText('drawer-title', '运行详情 · ' + (str(r.id) || '—'));

  const dl = document.createElement('dl');
  dl.className = 'kv';
  const kv = function (k, v) {
    const box = document.createElement('div');
    const dt = document.createElement('dt');
    dt.textContent = k;
    const dd = document.createElement('dd');
    dd.textContent = str(v);
    box.append(dt, dd);
    dl.append(box);
  };
  kv('状态', STATUS_TEXT[r.status] || str(r.status) || '—');
  kv('决策', decisionText(r.decision));
  kv('执行方式', modeText(r.mode));
  kv('生效档位', str(r.slot) || '—');
  kv('目标时刻', str(r.target) || '—');
  kv('开始', fmtDateTime(r.started_at));
  kv('结束', fmtDateTime(r.finished_at));
  kv('耗时', fmtLatency(r.latency_ms));
  kv('是否迟到', r.is_late === true ? '是' : '否');

  const loc = r.location || null;
  kv('坐标', loc ? (fmtNum(loc.lon) + ', ' + fmtNum(loc.lat)) : '—');
  kv('位置来源', loc ? (str(loc.label) || '—') : '—');
  kv('抖动', loc ? (loc.jittered ? '是' : '否') : '—');
  kv('上报文案', loc ? (str(loc.actual_location) || '—') : '—');

  const school = r.school || null;
  kv('校区', school ? ((str(school.campus) || '—') + (school.matched ? '（命中）' : '（未命中）')) : '—');

  const notify = r.notify || null;
  kv('推送', notify ? (notifyName(notify.channel) + (notify.ok ? ' 成功' : ' 失败')) : '未推送');
  body.append(dl);

  const errText = str(r.error);
  if (errText) {
    const box = document.createElement('div');
    // 间距交给 CSS（.err-box-gap）：CSP 下内联 style 属性本身会被 style-src 丢弃，
    // 而"文档里不出现 [style]"是可自动化门禁，不能在错误分支上破例。
    box.className = 'err-box err-box-gap';
    const p = document.createElement('p');
    p.className = 'err';
    p.textContent = '错误原文：' + errText;
    box.append(p);
    body.append(box);
  }

  const logs = Array.isArray(r.logs) ? r.logs : [];
  if (logs.length) {
    const h = document.createElement('h4');
    h.textContent = '运行日志（最多 20 行）';
    const ol = document.createElement('ol');
    ol.className = 'mono small';
    logs.forEach(function (line) {
      const li = document.createElement('li');
      li.textContent = str(line);
      ol.append(li);
    });
    body.append(h, ol);
  }
}

/* ---------- 10. 事件绑定 ---------- */

function bindEvents() {
  /* 启动重试 */
  const retry = $('btn-boot-retry');
  if (retry) retry.addEventListener('click', function () {
    retry.hidden = true;
    setText('boot-msg', '正在连接服务…');
    init();
  });

  /* 首次设置密码 */
  const setupForm = $('form-setup');
  if (setupForm) setupForm.addEventListener('submit', async function (ev) {
    ev.preventDefault();
    clearBox('setup-err');
    const p1 = $('setup-pass') ? $('setup-pass').value : '';
    const p2 = $('setup-pass2') ? $('setup-pass2').value : '';
    if (p1.length < 8) { showError('setup-err', { message: '密码至少 8 个字符' }); return; }
    if (p1 !== p2) { showError('setup-err', { message: '两次输入不一致，请重新输入' }); return; }
    const btn = $('setup-submit');
    setBusy(btn, '设置中…');
    try {
      await apiFetch('/auth/setup', { method: 'POST', body: { password: p1 } });
      await refreshSession().catch(function () {});
      if (!S.session.authed) {
        await apiFetch('/auth/login', { method: 'POST', body: { password: p1 } });
        await refreshSession();
      }
      if ($('setup-pass')) $('setup-pass').value = '';
      if ($('setup-pass2')) $('setup-pass2').value = '';
      toast('ok', '管理密码已设置');
      announce('管理密码已设置，已登录');
      enterApp();
    } catch (e) {
      showError('setup-err', e, '若提示已初始化（409），说明密码已经设过，直接登录即可。');
      if (e.status === 409) { await refreshSession().catch(function () {}); showAuth('login'); }
    } finally {
      clearBusy(btn);
    }
  });

  /* 登录 */
  const loginForm = $('form-login');
  if (loginForm) loginForm.addEventListener('submit', async function (ev) {
    ev.preventDefault();
    clearBox('login-err');
    const pw = $('login-pass') ? $('login-pass').value : '';
    if (!pw) { showError('login-err', { message: '请输入管理密码' }); return; }
    const btn = $('login-submit');
    setBusy(btn, '登录中…');
    try {
      await apiFetch('/auth/login', { method: 'POST', body: { password: pw } });
      if ($('login-pass')) $('login-pass').value = '';
      await refreshSession();
      enterApp();
      toast('ok', '已登录');
    } catch (e) {
      showError('login-err', e, e.status === 401 ? '同一 IP 15 分钟内失败 5 次会触发限速；确认密码后再试。' : '');
    } finally {
      clearBusy(btn);
      pumpRate();
    }
  });

  /* 退出 */
  const logout = $('btn-logout');
  if (logout) logout.addEventListener('click', async function () {
    try { await apiFetch('/auth/logout', { method: 'POST', quiet: true }); } catch (e) { /* ignore */ }
    S.session.authed = false;
    showAuth('login');
    toast('ok', '已退出登录');
  });

  /* 导航 */
  each(document.querySelectorAll('.nav-btn'), function (btn) {
    btn.addEventListener('click', function () { location.hash = btn.dataset.route; });
  });
  window.addEventListener('hashchange', onRoute);

  /* 仪表盘操作 */
  const force = $('btn-force');
  if (force) force.addEventListener('click', function () { startRun('force', false, force, '提交中…'); });
  const autoSwitch = $('btn-auto-switch');
  if (autoSwitch) autoSwitch.addEventListener('click', toggleAutoSwitch);
  const probe = $('btn-probe');
  if (probe) probe.addEventListener('click', function () { startRun('normal', true, probe, '预检中…'); });
  const notifyBtn = $('btn-notify');
  if (notifyBtn) notifyBtn.addEventListener('click', async function () {
    setBusy(notifyBtn, '推送中…');
    try {
      const r = await apiFetch('/notify/test', { method: 'POST' });
      toast('ok', '测试推送已发送（通道：' + (str(r && r.channel) || 'unknown') + '）');
      announce('测试推送已发送');
    } catch (e) {
      toastLocal(e);
    } finally {
      clearBusy(notifyBtn);
    }
  });

  /* 配置表单 */
  const cfgForm = $('form-config');
  if (cfgForm) {
    cfgForm.addEventListener('submit', function (ev) { ev.preventDefault(); saveConfig(); });
    cfgForm.addEventListener('input', function (ev) {
      const t = ev.target;
      if (!t || !t.classList) return;
      /* 凭据框（密码/token/推送 key）都会改动「已配置」徽标 */
      if (t.closest && t.closest('.has-secret')) applySecretBadges();
      syncCredNote();
      if (t.closest && t.closest('#pool-rows')) syncPoolNote();
    });
  }
  each(document.querySelectorAll('.peek'), function (btn) {
    setPeek(btn, false);
    /* 点击可能是异步的（要走二次校验），回调里不等待返回值：错误都在弹窗里就地展示 */
    btn.addEventListener('click', function () { peekSecret(btn); });
  });

  /* 显示凭据弹窗 */
  const revealOk = $('btn-reveal-ok');
  if (revealOk) revealOk.addEventListener('click', function () { submitReveal(revealOk); });
  const revealCancel = $('btn-reveal-cancel');
  if (revealCancel) revealCancel.addEventListener('click', closeReveal);
  const revealClose = $('btn-reveal-close');
  if (revealClose) revealClose.addEventListener('click', closeReveal);
  const revealModal = $('modal-reveal');
  if (revealModal) revealModal.addEventListener('click', function (ev) { if (ev.target === revealModal) closeReveal(); });
  const revealPass = $('reveal-pass');
  if (revealPass) {
    revealPass.addEventListener('keydown', function (ev) {
      if (ev.key !== 'Enter') return;
      ev.preventDefault();
      submitReveal(revealOk);
    });
  }

  /* 登录弹窗：学号密码与短信码都在这里，配置页只有一行摘要 + 一个入口 */
  const loginOpen = $('btn-login-open');
  if (loginOpen) loginOpen.addEventListener('click', function () { openLoginModal(); });
  const loginClose = $('btn-login-close');
  if (loginClose) loginClose.addEventListener('click', closeLoginModal);
  const loginModal = $('modal-login');
  if (loginModal) loginModal.addEventListener('click', function (ev) {
    if (ev.target === loginModal) closeLoginModal();
  });
  const loginBody = $('modal-login') ? $('modal-login').querySelector('.lg-body') : null;
  if (loginBody) {
    loginBody.addEventListener('input', function (ev) {
      const id = ev.target && ev.target.id;
      if (id === 'lg-username' || id === 'lg-password') mirrorAccount();
    });
    /* 弹窗不在 form-config 里，回车不会顺带保存整份配置：这里直接当「测试登录」 */
    loginBody.addEventListener('keydown', function (ev) {
      if (ev.key !== 'Enter') return;
      const id = ev.target && ev.target.id;
      if (id !== 'lg-username' && id !== 'lg-password') return;
      ev.preventDefault();
      testLogin($('btn-login-test'));
    });
  }
  const tokenTestBtn = $('btn-token-test');
  if (tokenTestBtn) tokenTestBtn.addEventListener('click', function () { testToken(tokenTestBtn); });
  const cookieClear = $('btn-cookie-clear');
  if (cookieClear) cookieClear.addEventListener('click', function () {
    S.dropCookie = true;
    syncCookieNote();
    toast('info', '保存后才会真正清掉那条备用 Cookie（现在还不影响服务）');
  });

  /* 改管理密码：这两个输入框在 form-config 里，回车默认会提交并保存整份配置，
     必须截住改成「改密码」——否则用户以为改了密码，实际只是又存了一次配置。 */
  const pwdBtn = $('btn-pwd-change');
  if (pwdBtn) {
    pwdBtn.addEventListener('click', function () { changePassword(pwdBtn); });
    ['pwd-current', 'pwd-new'].forEach(function (id) {
      const el = $(id);
      if (!el) return;
      el.addEventListener('keydown', function (ev) {
        if (ev.key !== 'Enter') return;
        ev.preventDefault();
        changePassword(pwdBtn);
      });
    });
  }
  /* 「保存」是 form="form-config" 的 submit 按钮：只挂在 submit 上，避免一次点击发两次 PUT */
  const importBtn = $('btn-config-import');
  if (importBtn) importBtn.addEventListener('click', openImport);
  const exportBtn = $('btn-config-export');
  if (exportBtn) exportBtn.addEventListener('click', exportConfig);
  const typeSel = $('cfg-notify-type');
  if (typeSel) typeSel.addEventListener('change', applyNotifyType);
  /* 测试登录：只发当前表单里的内容，不写配置（写配置仍然只能点「保存」） */
  const loginTestBtn = $('btn-login-test');
  if (loginTestBtn) loginTestBtn.addEventListener('click', function () { testLogin(loginTestBtn); });
  const loginRefreshBtn = $('btn-login-refresh');
  if (loginRefreshBtn) loginRefreshBtn.addEventListener('click', function () { refreshToken(loginRefreshBtn); });
  /* 短信验证码：入口只在学校回「要二次验证」之后出现，点了才真的发短信 */
  const smsStartBtn = $('btn-sms-start');
  if (smsStartBtn) smsStartBtn.addEventListener('click', function () { smsStart(smsStartBtn); });
  const smsSendBtn = $('btn-sms-send');
  if (smsSendBtn) smsSendBtn.addEventListener('click', function () {
    if (smsTicket) smsResend(smsSendBtn); else smsStart(smsSendBtn);
  });
  const smsVerifyBtn = $('btn-sms-verify');
  if (smsVerifyBtn) smsVerifyBtn.addEventListener('click', function () { smsVerify(); });
  const smsCancelBtn = $('btn-sms-cancel');
  if (smsCancelBtn) smsCancelBtn.addEventListener('click', smsCancel);
  const smsCodeInput = $('sms-code');
  if (smsCodeInput) {
    /* 只留数字、最多 6 位：粘贴一整条短信也能截出前 6 位数字。
       洗完就交给 smsSchedule()：凑满 6 位 250ms 后自动提交，不用再点按钮。 */
    smsCodeInput.addEventListener('input', function () {
      const clean = String(smsCodeInput.value).replace(/[^0-9]/g, '').slice(0, SMS_CODE_LEN);
      if (clean !== smsCodeInput.value) smsCodeInput.value = clean;
      smsSchedule();
    });
    /* 弹窗里回车不提交任何表单，这里保留「回车即提交」的手感 */
    smsCodeInput.addEventListener('keydown', function (ev) {
      if (ev.key !== 'Enter') return;
      ev.preventDefault();
      smsVerify();
    });
  }
  /* 浏览器定位：按钮在「位置」区域，未登录/视图隐藏时可能压根不存在，必须判空 */
  const geoBtn = $('btn-geo');
  if (geoBtn) geoBtn.addEventListener('click', function () { locateNow(geoBtn); });
  /* 地图选点：单个坐标一个按钮，位置池每行一个；行是动态加的，所以走事件委托 */
  const mapPickBtn = $('btn-map-pick');
  if (mapPickBtn) mapPickBtn.addEventListener('click', function () { openMapPicker(null); });
  const poolPickBox = $('pool-rows');
  if (poolPickBox) poolPickBox.addEventListener('click', function (ev) {
    const btn = ev.target && ev.target.closest ? ev.target.closest('.r-pick') : null;
    if (btn) openMapPicker(btn.closest('.row'));
  });

  each(document.querySelectorAll('.clear-secret'), function (btn) {
    btn.addEventListener('click', function () {
      const target = $(btn.dataset.target);
      if (!target) return;
      target.value = '';
      /* 弹窗里的「清空」清的是镜像框，得同步回隐藏域，否则保存时又把旧值写回去 */
      if (btn.dataset.target === 'lg-password') mirrorAccount();
      applySecretBadges();
      toast('info', '已清空该字段：保存后才会真正移除（不保存则不影响服务）');
    });
  });

  const addPool = $('btn-pool-add');
  if (addPool) addPool.addEventListener('click', function () { addPoolRow({}); syncPoolNote(); });
  const addTime = $('btn-time-add');
  if (addTime) addTime.addEventListener('click', function () { addTimeRow({}); });
  const addVac = $('btn-vac-add');
  if (addVac) addVac.addEventListener('click', function () { addVacRow({}); });

  bindRowDelete('pool-rows', '.r-del', syncPoolNote);
  bindRowDelete('time-rows', '.t-del', null);
  bindRowDelete('vac-rows', '.v-del', null);

  /* 导入弹窗 */
  const previewBtn = $('btn-import-preview');
  if (previewBtn) previewBtn.addEventListener('click', previewImport);
  const applyBtn = $('btn-import-apply');
  if (applyBtn) applyBtn.addEventListener('click', applyImport);
  const closeImportBtn = $('btn-import-close');
  if (closeImportBtn) closeImportBtn.addEventListener('click', closeImport);

  /* 日志 */
  const levelSel = $('log-level');
  if (levelSel) levelSel.addEventListener('change', function () {
    const list = $('logs-list');
    if (list) list.dataset.level = levelSel.value || 'all';
  });
  const auto = $('log-autoscroll');
  if (auto) {
    S.autoScroll = auto.checked;
    auto.addEventListener('change', function () { S.autoScroll = auto.checked; });
  }
  const clearBtn = $('btn-log-clear');
  if (clearBtn) clearBtn.addEventListener('click', clearLogs);
  const copyBtn = $('btn-log-copy');
  if (copyBtn) copyBtn.addEventListener('click', copyLogs);

  /* 历史 */
  const limitSel = $('runs-limit');
  if (limitSel) limitSel.addEventListener('change', function () {
    S.runsPageSize = Number(limitSel.value) || 20;
    S.runsPages = 1;
    loadRuns(false);
  });
  const moreBtn = $('btn-runs-more');
  if (moreBtn) moreBtn.addEventListener('click', function () {
    S.runsPages += 1;
    setBusy(moreBtn, '加载中…');
    loadRuns(false);
  });
  const runsList = $('runs-list');
  if (runsList) runsList.addEventListener('click', function (ev) {
    const btn = ev.target && ev.target.closest ? ev.target.closest('.run-item') : null;
    if (btn) openRun(btn.dataset.id);
  });

  /* 覆盖层关闭 */
  const drawer = $('drawer');
  if (drawer) drawer.addEventListener('click', function (ev) { if (ev.target === drawer) closeDrawer(); });
  const drawerClose = $('drawer-close');
  if (drawerClose) drawerClose.addEventListener('click', closeDrawer);
  const modal = $('modal-import');
  if (modal) modal.addEventListener('click', function (ev) { if (ev.target === modal) closeImport(); });

  document.addEventListener('keydown', function (ev) {
    if (ev.key !== 'Escape') return;
    /* 显示凭据叠在登录弹窗上（弹窗里点眼睛），先收最上面那张 */
    if (!isHidden('modal-reveal')) { closeReveal(); return; }
    if (!isHidden('modal-import')) { closeImport(); return; }
    if (!isHidden('modal-login')) { closeLoginModal(); return; }
    if (!isHidden('drawer')) closeDrawer();
  });
}

function bindRowDelete(boxId, sel, after) {
  const box = $(boxId);
  if (!box) return;
  box.addEventListener('click', function (ev) {
    const btn = ev.target && ev.target.closest ? ev.target.closest(sel) : null;
    if (!btn) return;
    const row = btn.closest('.row');
    if (row) row.remove();
    if (after) after();
  });
}

/* ---------- 11. 启动 ---------- */

async function init() {
  if (location.protocol === 'file:') {
    setHidden('booting', true);
    setHidden('file-notice', false);
    return;
  }
  setHidden('booting', false);
  setHidden('file-notice', true);
  setHidden('auth', true);
  setHidden('shell', true);
  try {
    await refreshSession();
  } catch (e) {
    setText('boot-msg', '无法连接服务：' + (e.message || '未知错误'));
    setHidden('btn-boot-retry', false);
    return;
  }
  setHidden('booting', true);
  if (!S.session.initialized) showAuth('setup');
  else if (!S.session.authed) showAuth('login');
  else enterApp();
}

bindEvents();
init();

/* ---- 外观：跟随系统 / 浅色 / 深色（localStorage 记一次选择） ---- */
(function initTheme() {
  const KEY = 'fzu.theme';
  const root = document.documentElement;
  const btn = document.getElementById('btn-theme');
  const modes = ['auto', 'light', 'dark'];
  const label = { auto: '跟随系统', light: '浅色', dark: '深色' };
  function apply(mode) {
    if (mode === 'auto') root.removeAttribute('data-theme');
    else root.setAttribute('data-theme', mode);
    if (btn) btn.textContent = label[mode];
  }
  let saved = 'auto';
  try { saved = localStorage.getItem(KEY) || 'auto'; } catch (e) { saved = 'auto'; }
  if (modes.indexOf(saved) < 0) saved = 'auto';
  apply(saved);
  if (!btn) return;
  btn.addEventListener('click', function () {
    saved = modes[(modes.indexOf(saved) + 1) % modes.length];
    try { localStorage.setItem(KEY, saved); } catch (e) { /* 隐私模式下忽略 */ }
    apply(saved);
  });
})();

