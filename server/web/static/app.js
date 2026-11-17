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
  /* 配置 */
  config: null, configVersion: 0, configLoaded: false, importPreview: null, maskedFields: {},
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
function isHidden(id) { const el = $(id); return !el || el.hidden; }

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
  } else if (res.status === 429) {
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
  S.config = null; S.configVersion = 0; S.configLoaded = false; S.importPreview = null; S.maskedFields = {};
  S.logs = []; S.logKeys = new Set(); S.logsSnapshotted = false;
  S.runs = []; S.runsPages = 1; S.runsExhausted = false;
}

function showAuth(mode) {
  stopDashboardLoop();
  closeStream();
  resetData();
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
  loginTestStatus('已从链接取得免登凭证，正在校验…', '');
  try {
    /* 先验证再保存：和「获取并保存凭证」同一条规矩，不把不能用的东西写进配置 */
    const probe = await apiFetch('/login/test', { method: 'POST', body: { token: token }, keepSession: true, quiet: true });
    const r = await apiFetch('/config', { method: 'PUT', body: { config: cfg } });
    if (r && r.version !== undefined && r.version !== null) pinVersion(r.version);
    if (r && Array.isArray(r.warnings) && r.warnings.length) showWarnings('config-warnings', r.warnings);
    S.configLoaded = false;
    await loadConfig(true);
    const parts = ['免登凭证已自动保存 ' + str(probe.token_hint)];
    if (str(probe.plan)) parts.push(str(probe.plan));
    loginTestStatus(parts.join(' · '), 'ok');
    announce('已自动保存免登凭证');
  } catch (e) {
    setVal('cfg-token', token);
    /* 只取第一行：校验失败时 e.message 后面会跟一大段逐条规则，塞进状态行就没法看了。
       凭证已经填进输入框，用户补完其它项点「保存」即可，不用再跑一遍脚本。 */
    const first = String((e && e.message) || '保存失败').split('\n')[0];
    loginTestStatus('已取到凭证但没能自动保存：' + first + '（凭证已填进「免登凭证」框，补完其它项后点「保存」即可）', 'err');
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
  setText('dash-version', 'v' + (str(st.version) || '—') + ' · 配置 v' + (st.config_version === undefined || st.config_version === null ? '—' : str(st.config_version)));
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
    setText('dash-next-at', '没有后续档位（今日计划已结束，或者设置的签到时刻都落在签到窗口之外）');
    return;
  }
  const now = Date.now() + (S.skew || 0);
  const left = Math.max(0, Math.floor((target - now) / 1000));
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
    const msg = e.status === 409 ? '已有一次运行正在进行中，等它结束后再试。' : ('提交失败：' + (e.message || ('HTTP ' + e.status)));
    setText('dash-action-hint', msg);
    toastLocal(e);
  } finally {
    clearBusy(btn);
    updateActionState((S.status || {}).scheduler);
  }
}

/* ---------- 6. 配置 ---------- */

/* 保存成功后以 PUT 返回的版本为准：GET 回显可能因缓存/时序落后一步 */
function pinVersion(version) {
  if (version === null || version === undefined) return;
  S.configVersion = version;
  setText('config-version', '配置版本 v' + str(version));
}

async function loadConfig(force) {
  if (S.configLoaded && !force) return;
  try {
    const d = await apiFetch('/config');
    S.config = (d && d.config) ? d.config : {};
    S.maskedFields = (d && d.masked_fields) ? d.masked_fields : {};
    S.configVersion = (d && d.version !== undefined && d.version !== null) ? d.version : (S.config.version || 0);
    S.configLoaded = true;
    fillConfig(S.config);
    clearBox('config-msg');
    showWarnings('config-warnings', (d && d.warnings) || []);
    setText('config-version', '配置版本 v' + (str(S.configVersion) || '—') + (d && d.source ? ' · 来源 ' + str(d.source) : ''));
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
  setVal('cfg-user-cookie', user.cookie);

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

  syncPoolNote();
  applyNotifyType();
  applySecretBadges();
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
    /* Cookie 是 textarea，password/token 是 input：两种都要能挂「已配置」徽标 */
    const input = wrap.querySelector('input, textarea');
    const badge = wrap.querySelector('.mask-badge');
    if (!input || !badge) return;
    badge.hidden = !(input.value.trim() !== '' || S.maskedFields[wrap.dataset.secret]);
  });
}

function collectConfig() {
  const cur = S.config || {};
  return {
    version: Number(S.configVersion) || Number(cur.version) || 0,
    user: {
      username: getVal('cfg-username'),
      password: getVal('cfg-password'),
      token: getVal('cfg-token'),
      cookie: getVal('cfg-user-cookie')
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

  const hasToken = !!cfg.user.token;
  const hasPw = !!(cfg.user.username && cfg.user.password);
  /* Cookie 也是凭据：只填 Cookie 的人不该被本地校验拦住（脱敏哨兵同样算"已配置"） */
  const hasCookie = !!cfg.user.cookie;
  if (!hasToken && !hasPw && !hasCookie) p.push('凭据为空：填浏览器 Cookie，或填「学号 + 密码」，或填免登凭证（三选一）。');

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
    const saved = (r && r.version !== undefined && r.version !== null) ? r.version : null;
    S.config = cfg;
    S.configLoaded = true;
    await loadConfig(true);
    if (saved !== null) pinVersion(saved);
    if (r && Array.isArray(r.warnings) && r.warnings.length) showWarnings('config-warnings', r.warnings);
    toast('ok', '配置已保存（v' + (str(S.configVersion) || '?') + '）');
    announce('配置已保存');
    loadDashboard(true);
  } catch (e) {
    showError('config-msg', e, '按上面逐条提示修改后再保存；错误原文来自服务端校验（与 YAML 导入同一套规则）。');
  } finally {
    clearBusy(btn);
  }
}

/* ---------- 6.1 「测试登录」：POST /login/test ---------- */

const LOGIN_MODE_TEXT = { cookie: 'Cookie', password: '学号 + 密码', token: '免登凭证' };

/* 明文回显时就是原样发送；框里还留着旧服务端的脱敏哨兵时换成空串（服务端不认识
   哨兵，空串会让它回落到已保存的值，而不是把哨兵当真凭据）。 */
function loginTestBody() {
  const pick = function (id) {
    const v = getVal(id);
    return maskedLike(v) ? '' : v;
  };
  return {
    cookie: pick('cfg-user-cookie'),
    username: pick('cfg-username'),
    password: pick('cfg-password'),
    token: pick('cfg-token')
  };
}

function loginTestStatus(text, kind) {
  const el = $('login-test-status');
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

async function testLogin(btn) {
  const btnEl = btn || $('btn-login-test');
  /* 框里是旧服务端回传的脱敏哨兵就不发送它（服务端不认识哨兵）：loginTestBody()
     会换成空串，服务端随即回落到已保存的那份 Cookie——这恰好是最常问的问题
     「我上次存的 Cookie 还能用吗」，所以这里不能直接拦住不测。 */
  const usingSavedCookie = maskedLike(getVal('cfg-user-cookie'));
  setBusy(btnEl, '测试中…');
  loginTestStatus(usingSavedCookie ? '正在用已保存的 Cookie 试登录…' : '正在用当前填写的内容试登录…', '');
  try {
    /* quiet：结论就在下面这一行显示，别再叠一个 toast；
       keepSession：登录失败可能回 502/401，那是「这条凭据不通」，不是管理端会话失效。 */
    const r = await apiFetch('/login/test', { method: 'POST', body: loginTestBody(), keepSession: true, quiet: true });
    loginTestStatus(loginTestResultText(r), 'ok');
    announce('测试登录成功');
  } catch (e) {
    loginTestStatus(loginTestErrorText(e), 'err');
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
  setBusy(btnEl, '获取中…');
  loginTestStatus('正在用当前凭据换新免登凭证…', '');
  try {
    const r = await apiFetch('/login/refresh', { method: 'POST', body: loginTestBody(), keepSession: true, quiet: true });
    const parts = ['已获取免登凭证 ' + str(r.token_hint) + '，已写入配置'];
    if (str(r.plan)) parts.push(str(r.plan));
    if (r.cookie_saved === true) parts.push('浏览器 Cookie 也一并保存了');
    loginTestStatus(parts.join(' · '), 'ok');
    announce('已获取免登凭证');
    /* 重新拉一次配置：token 框与「已配置」徽标要跟着变，否则界面上还是旧状态 */
    S.configLoaded = false;
    await loadConfig(true);
  } catch (e) {
    loginTestStatus(loginTestErrorText(e), 'err');
  } finally {
    clearBusy(btnEl);
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
  setHidden('modal-import', false);
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
  const body = Object.assign({}, S.importPreview, { version: Number(S.configVersion) || 0 });
  setBusy(btn, '保存中…');
  try {
    const r = await apiFetch('/config', { method: 'PUT', body: { config: body } });
    const saved = (r && r.version !== undefined && r.version !== null) ? r.version : null;
    toast('ok', '已应用导入的配置（v' + (saved === null ? '?' : str(saved)) + '）');
    announce('已应用导入的配置');
    closeImport();
    await loadConfig(true);
    if (saved !== null) pinVersion(saved);
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
  setHidden('drawer', false);
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
      /* 凭据框（密码/token/推送 key 的 input 与 Cookie 的 textarea）都会改动「已配置」徽标 */
      if (t.closest && t.closest('.has-secret')) applySecretBadges();
      if (t.closest && t.closest('#pool-rows')) syncPoolNote();
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
    if (!isHidden('modal-import')) { closeImport(); return; }
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
