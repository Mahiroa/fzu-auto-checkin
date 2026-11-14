/* =============================================================================
 * fzu-auto-checkin · 管理前端（零构建 / 零依赖 / 原生 ES module）
 *
 * 数据流总览：status → 视图 → 操作 → 重新拉取
 *   1) 启动   GET /auth/session 决定三态之一：设置密码 / 登录 / 进入应用。
 *               未初始化强制「设置管理密码」；未登录只显示登录卡片。
 *   2) 仪表盘 #/dashboard：每 15 s 拉 GET /status + GET /runs?limit=5；
 *              倒计时用 next_run_at 在本地每秒递减（不额外轮询接口）。
 *              按钮 → POST /runs {mode} / POST /notify/test → 重新拉 status+runs。
 *   3) 配置   #/config：GET /config（脱敏）→ fillConfig 填表 → PUT /config {config}；
 *              导入走 POST /config/import-yaml 预览 → PUT 应用；
 *              导出是 GET /config/export?format=yaml 附件下载。
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
  serverchan: 'Server酱 Turbo：填 SCT 开头的 SendKey，成功判据是 HTTP 200 且 code=0。',
  pushplus: 'PushPlus：填 token，成功判据是 HTTP 200 且 code=200。',
  bark: 'Bark：填完整的推送地址（含你的 device key）。',
  wecom: '企业微信群机器人：填 webhook 完整地址，成功判据是 errcode=0。',
  none: '不推送：签到结论只落在运行历史与日志里。'
};
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
  config: null, configVersion: 0, configLoaded: false, importPreview: null,
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

function setBusy(btn, text) {
  if (!btn) return;
  if (!btn.dataset.label) btn.dataset.label = btn.textContent;
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
  delete btn.dataset.busy;
  btn.disabled = false;
  btn.removeAttribute('aria-busy');
  if (btn.dataset.label) btn.textContent = btn.dataset.label;
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
    S.session.authed = false;
    showAuth('login');
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
  S.config = null; S.configVersion = 0; S.configLoaded = false; S.importPreview = null;
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

function enterApp() {
  S.session.authed = true;
  setHidden('auth', true);
  setHidden('booting', true);
  setHidden('file-notice', true);
  setHidden('shell', false);
  if (ROUTES.indexOf(location.hash) < 0) { location.hash = '#/dashboard'; return; }
  onRoute();
}

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
    setText('dash-next-at', '没有后续档位（今日计划已结束，或 times 全在窗口外）');
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
    toast('ok', what + '已受理' + (id ? '（run_id=' + id + '）' : '') + '，结果见日志与推送');
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

  setVal('cfg-lon', ck.longitude);
  setVal('cfg-lat', ck.latitude);
  setVal('cfg-actual', ck.actual_location);
  setVal('cfg-jitter', numStr(ck.location_jitter_m));
  setVal('cfg-jitter-legacy', numStr(ck.jitter_radius_m));

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
      note.textContent = '位置池已有 ' + n + ' 项：上面的单点经度 / 纬度 / 上报文案已被位置池接管，不再生效（每次按 weight 加权随机挑一个点）。';
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
    const input = wrap.querySelector('input');
    const badge = wrap.querySelector('.mask-badge');
    if (!input || !badge) return;
    badge.hidden = !maskedLike(input.value);
  });
}

function collectConfig() {
  const cur = S.config || {};
  return {
    version: Number(S.configVersion) || Number(cur.version) || 0,
    user: {
      username: getVal('cfg-username'),
      password: getVal('cfg-password'),
      token: getVal('cfg-token')
    },
    checkin: {
      longitude: getVal('cfg-lon'),
      latitude: getVal('cfg-lat'),
      actual_location: getVal('cfg-actual'),
      locations: readPool(),
      location_jitter_m: numOrNull(getVal('cfg-jitter')),
      jitter_radius_m: numOrNull(getVal('cfg-jitter-legacy')),
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
  if (!hasToken && !hasPw) p.push('凭据为空：填免登 token，或填「学号 + 密码」（推荐后者，token 失效可自动换新）。');

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

  previewLine(ul, '窗口：' + (str(sch.window_start) || '默认') + ' – ' + (str(sch.window_end) || '默认'));
  previewLine(ul, '档位：' + (times.length ? times.join(' / ') : '默认 21:35'));
  previewLine(ul, '位置：' + (locs.length ? ('位置池 ' + locs.length + ' 项') : ((str(ck.longitude) || '?') + ', ' + (str(ck.latitude) || '?'))));
  previewLine(ul, '抖动：' + (numStr(ck.location_jitter_m) || numStr(ck.jitter_radius_m) || '默认 200') + ' 米 · 时间抖动 ±' + (numStr(sch.time_jitter_minutes) || '0') + ' 分钟');
  previewLine(ul, '等待 / 容忍：' + (numStr(sch.max_wait_minutes) || '12') + ' / ' + (numStr(sch.tolerance_minutes) || '5') + ' 分钟');
  previewLine(ul, '推送：' + (str(nt.type) || 'none'));
  previewLine(ul, '假期：' + (ranges.length
    ? ranges.length + ' 段 · ' + ranges.map(function (r) { return (str(r.name) || '未命名') + ' ' + (str(r.start) || '?') + '~' + (str(r.end) || '?'); }).join('；')
    : '无'));
  previewLine(ul, '凭据：token ' + (user.token ? '已填' : '空') + ' · 学号 ' + (user.username ? '已填' : '空') + ' · 密码 ' + (user.password ? '已填' : '空'));
  previewLine(ul, '服务：' + (str(srv.listen) || '默认') + ' · 数据目录 ' + (str(srv.data_dir) || '默认'));
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
  if (e && e.run_id) li.title = 'run_id=' + str(e.run_id);
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
  try {
    const d = await apiFetch('/runs?limit=' + runsLimit(), { quiet: quiet });
    S.runs = (d && Array.isArray(d.runs)) ? d.runs : [];
    S.runsExhausted = S.runs.length < runsLimit();
    renderRuns();
  } catch (e) {
    if (e.status !== 401 && !quiet) toast('error', e.message || '读取运行历史失败');
    else if (e.status !== 401) setText('runs-count', '读取失败，稍后重试');
  } finally {
    clearBusy(btn);
  }
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
    tail.textContent = errText || ('decision ' + (str(r && r.decision) || '—'));

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
  kv('decision', str(r.decision) || '—');
  kv('mode', str(r.mode) || '—');
  kv('slot', str(r.slot) || '—');
  kv('target', str(r.target) || '—');
  kv('开始', fmtDateTime(r.started_at));
  kv('结束', fmtDateTime(r.finished_at));
  kv('latency_ms', fmtLatency(r.latency_ms));
  kv('is_late', r.is_late === true ? '是' : '否');

  const loc = r.location || null;
  kv('坐标', loc ? (fmtNum(loc.lon) + ', ' + fmtNum(loc.lat)) : '—');
  kv('位置来源', loc ? (str(loc.label) || '—') : '—');
  kv('抖动', loc ? (loc.jittered ? '是' : '否') : '—');
  kv('上报文案', loc ? (str(loc.actual_location) || '—') : '—');

  const school = r.school || null;
  kv('校区', school ? ((str(school.campus) || '—') + (school.matched ? '（命中）' : '（未命中）')) : '—');

  const notify = r.notify || null;
  kv('推送', notify ? ((str(notify.channel) || '—') + (notify.ok ? ' 成功' : ' 失败')) : '未推送');
  body.append(dl);

  const errText = str(r.error);
  if (errText) {
    const box = document.createElement('div');
    box.className = 'err-box';
    box.style.marginTop = '10px';
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
      if (t.type === 'password') applySecretBadges();
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
