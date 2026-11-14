// 真实浏览器验收：Chrome + CDP（flat session），跑完四个视图并留截图。
//
// 为什么单独写一个脚本而不是手点：验收结论要可复现、可留痕。这里把「登录 → 布局 →
// 仪表盘 → 配置（含自绘控件 + 浏览器定位）→ 日志(SSE) → 历史 → 移动端 → 控制台/CSP 干净」
// 每一步都落成断言 + PNG，失败会以非 0 退出码返回。
// 硬约束（见 browser-verification 技能）：只用 Google Chrome 绝对路径，绝不用 Edge。
//
// 用法：node scripts/ui-verify.mjs --base http://127.0.0.1:8787 --cdp 9225 --password xxx --out <dir>
//
// v1.1（第二轮验收）新增：
//   - 左导航 / 右内容的 Grid 布局断言（1440×900），并真的点一次导航看 aria-current；
//   - 自绘控件的 computed appearance（select/checkbox/time/text）、checkbox 尺寸、全文档无内联 style；
//   - 浏览器定位：CDP 授权 + 坐标覆盖 → 断言 WGS-84→GCJ-02 的参考向量、精度提示、拒绝分支；
//   - 移动端（390×844）：无横向溢出 + 侧栏仍是 sticky + 导航条在主内容之上；
//   - CSP 违规（securitypolicyviolation / Log 域的 security error）与 console.error 一起必须为 0。

import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const args = Object.fromEntries(
  process.argv.slice(2).reduce((acc, cur, i, arr) => {
    if (cur.startsWith('--')) acc.push([cur.slice(2), arr[i + 1]]);
    return acc;
  }, [])
);
const BASE = args.base ?? 'http://127.0.0.1:8787';
const CDP = Number(args.cdp ?? 9225);
const PASSWORD = args.password ?? '';
const OUT = args.out ?? '.';
mkdirSync(OUT, { recursive: true });

let pass = 0;
const failures = [];
function check(ok, name, detail = '') {
  if (ok) { pass++; console.log(`  [PASS] ${name}`); }
  else { failures.push(name); console.log(`  [FAIL] ${name} ${detail}`); }
}

const version = await (await fetch(`http://127.0.0.1:${CDP}/json/version`)).json();
console.log(`浏览器: ${version.Browser}  (CDP ${CDP})`);

const ws = new WebSocket(version.webSocketDebuggerUrl);
await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });

let seq = 0;
const pending = new Map();
const consoleErrors = [];
const exceptions = [];
const cspViolations = [];

ws.onmessage = (raw) => {
  const msg = JSON.parse(raw.data);
  if (msg.id && pending.has(msg.id)) {
    const { resolve, reject } = pending.get(msg.id);
    pending.delete(msg.id);
    msg.error ? reject(new Error(JSON.stringify(msg.error))) : resolve(msg.result);
    return;
  }
  if (msg.method === 'Runtime.consoleAPICalled' && msg.params.type === 'error') {
    consoleErrors.push(msg.params.args.map((a) => a.value ?? a.description ?? '').join(' '));
  }
  if (msg.method === 'Runtime.exceptionThrown') {
    exceptions.push(msg.params.exceptionDetails.text + ' ' + (msg.params.exceptionDetails.exception?.description ?? ''));
  }
  // CSP 违规在 Chrome 里走 Log 域（source=security），不走 console API；
  // 只认"安全策略"这一类，网络层的 4xx/5xx 噪音不算。
  if (msg.method === 'Log.entryAdded' && msg.params.entry.level === 'error') {
    const e = msg.params.entry;
    if (e.source === 'security' || /Refused to|Content Security Policy|violates the following/i.test(e.text ?? '')) {
      cspViolations.push(`${e.source}: ${e.text}`);
    }
  }
};

function send(method, params = {}, sessionId) {
  const id = ++seq;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ id, method, params, sessionId }));
  });
}

const targets = await send('Target.getTargets');
const page = targets.targetInfos.find((t) => t.type === 'page');
if (!page) throw new Error('没有可用的 page target');
const { sessionId } = await send('Target.attachToTarget', { targetId: page.targetId, flatten: true });
const T = (m, p) => send(m, p, sessionId);

await T('Page.enable');
await T('Runtime.enable');
try { await T('Log.enable'); } catch { /* 老版本 Chrome 没有 Log 域，页面内监听兜底 */ }
// 页面内监听：securitypolicyviolation 是 CSP 违规最权威的来源，且不受 Log 域开关影响。
// 必须在 Page.navigate 之前注册，才能在文档解析阶段就捕获。
try {
  await T('Page.addScriptToEvaluateOnNewDocument', {
    source: `window.__cspViolations = [];
      document.addEventListener('securitypolicyviolation', function (e) {
        window.__cspViolations.push((e.effectiveDirective || e.violatedDirective) + ' ' + (e.blockedURI || ''));
      });`,
  });
} catch { /* 忽略：没有这个能力时只靠 Log 域 */ }
// 无头页面默认按 hidden 处理，setInterval 会被节流到约 1 次/分钟；提到前台并模拟焦点，
// 才能真实验证「倒计时每秒递减」这种依赖定时器的行为。
await T('Page.bringToFront', {});
try { await T('Emulation.setFocusEmulationEnabled', { enabled: true }); } catch { /* 旧版 Chrome 无此命令 */ }

async function evaluate(expression) {
  const r = await T('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error('JS 异常: ' + JSON.stringify(r.exceptionDetails.exception?.description ?? r.exceptionDetails));
  return r.result.value;
}

async function waitFor(expression, { timeout = 10000, label = expression } = {}) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    try { if (await evaluate(expression)) return true; } catch { /* 页面切换中，重试 */ }
    await new Promise((r) => setTimeout(r, 120));
  }
  throw new Error(`等待超时: ${label}`);
}

async function shot(name) {
  const { data } = await T('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
  const file = join(OUT, `${name}.png`);
  writeFileSync(file, Buffer.from(data, 'base64'));
  console.log(`  [SHOT] ${file}`);
}

async function setViewport(width, height, mobile = false) {
  await T('Emulation.setDeviceMetricsOverride', {
    width, height, deviceScaleFactor: 1, mobile,
    screenWidth: width, screenHeight: height,
  });
}

const text = (id) => evaluate(`(document.getElementById(${JSON.stringify(id)})?.innerText ?? '').trim()`);
const hidden = (id) => evaluate(`document.getElementById(${JSON.stringify(id)})?.hidden ?? true`);
const styleOf = (selector, prop) =>
  evaluate(`getComputedStyle(document.querySelector(${JSON.stringify(selector)})).getPropertyValue(${JSON.stringify(prop)})`);
const rectOf = (selector) =>
  evaluate(`(() => { const r = document.querySelector(${JSON.stringify(selector)})?.getBoundingClientRect();
    return r ? { left: r.left, top: r.top, right: r.right, bottom: r.bottom, width: r.width, height: r.height } : null; })()`);
const near = (got, want, eps) => Number.isFinite(Number(got)) && Math.abs(Number(got) - want) <= eps;

// 点 #btn-geo，等状态行从「申请定位权限…」落定，然后把状态 + 回填值一起取回来。
async function clickGeoSettle(label) {
  await evaluate(`(() => {
    const el = document.getElementById('geo-status');
    if (el) { el.textContent = ''; el.classList.remove('ok', 'err', 'busy'); }
    document.getElementById('btn-geo').click();
    return true;
  })()`);
  await waitFor(
    `(() => { const el = document.getElementById('geo-status'); const t = (el?.innerText ?? '').trim();
      return t.length > 0 && !/申请定位权限|定位中/.test(t); })()`,
    { label, timeout: 25000 }
  );
  return evaluate(`(() => {
    const el = document.getElementById('geo-status');
    return {
      text: (el.innerText || '').trim(),
      cls: el.className,
      lon: document.getElementById('cfg-lon').value,
      lat: document.getElementById('cfg-lat').value,
      version: (document.getElementById('config-version')?.innerText ?? '').trim(),
    };
  })()`);
}

try {
  // ---------- 登录 ----------
  await setViewport(1440, 900);
  await T('Page.navigate', { url: BASE + '/' });
  await waitFor(`document.readyState === 'complete'`, { label: '首屏加载' });
  await waitFor(`!document.getElementById('auth')?.hidden || !document.getElementById('shell')?.hidden`, { label: '鉴权门出现' });

  const already = await evaluate(`!document.getElementById('shell')?.hidden`);
  if (!already) {
    console.log('\n=== 登录 ===');
    await evaluate(`(() => {
      const input = document.getElementById('login-pass');
      input.value = ${JSON.stringify(PASSWORD)};
      input.dispatchEvent(new Event('input', { bubbles: true }));
      document.getElementById('form-login').requestSubmit();
      return true;
    })()`);
    await waitFor(`!document.getElementById('shell')?.hidden`, { label: '登录成功（shell 可见）' });
    check(true, '密码登录后进入应用外壳');
  } else {
    check(true, '已处于登录态（Cookie 复用）');
  }

  // ---------- 布局：左导航 + 右内容（1440×900） ----------
  console.log('\n=== 布局（1440×900）===');
  await evaluate(`window.scrollTo(0, 0)`);
  await sleep(150);

  const shellDisplay = await styleOf('#shell', 'display');
  check(shellDisplay === 'grid', '#shell 是 CSS Grid', shellDisplay);
  // computed grid-template-columns 在 Chrome 里已经解析成像素值（如 "248px 1192px"）。
  const firstCol = await evaluate(`parseFloat(
    getComputedStyle(document.getElementById('shell')).gridTemplateColumns.split(' ')[0] || '0')`);
  check(Number.isFinite(firstCol) && firstCol >= 200, '左侧栏宽度 ≥ 200px（§13 规定 248px）', `${firstCol}px`);

  const asidePos = await styleOf('aside.sidebar', 'position');
  check(asidePos === 'sticky' || asidePos === 'fixed', 'aside.sidebar 为 sticky/fixed（长页面滚动时导航不跑）', asidePos);

  const navRect = await rectOf('#nav');
  const mainRect = await rectOf('#main');
  if (navRect && mainRect) {
    check(navRect.left < mainRect.left, '#nav 在 #main 左侧', `nav.left=${navRect.left} main.left=${mainRect.left}`);
    console.log(`  [INFO] 左栏=${firstCol}px · aside.position=${asidePos} · nav.left=${navRect.left} · main.left=${mainRect.left}`);
  } else {
    check(false, '#nav / #main 都取得到几何信息', JSON.stringify({ navRect, mainRect }));
  }

  const navBtnCount = await evaluate(`document.querySelectorAll('aside .nav-btn').length`);
  check(navBtnCount === 4, '侧栏 4 个导航项', String(navBtnCount));

  // 真点一次「配置」：顺带验证 hash 路由 + aria-current 的联动（不是只看静态 DOM）。
  await evaluate(`(() => {
    const btn = Array.from(document.querySelectorAll('#nav .nav-btn')).find((b) => b.textContent.trim() === '配置');
    if (!btn) return false;
    btn.click();
    return true;
  })()`);
  await waitFor(`!document.getElementById('view-config')?.hidden`, { label: '点「配置」后配置视图可见' });
  const currentNav = await evaluate(
    `(document.querySelector('aside .nav-btn[aria-current="page"]')?.textContent ?? '').trim()`);
  check(currentNav === '配置', '点「配置」后侧栏高亮项是「配置」', `aria-current=${currentNav}`);
  await shot('verify-layout-sidebar');

  // 回到仪表盘，后续用例继续用真实导航（而不是只改 location.hash）。
  await evaluate(`(() => {
    const btn = Array.from(document.querySelectorAll('#nav .nav-btn')).find((b) => b.textContent.trim() === '仪表盘');
    if (btn) btn.click();
    return true;
  })()`);
  await waitFor(`!document.getElementById('view-dashboard')?.hidden`, { label: '回到仪表盘' });

  // ---------- 仪表盘 ----------
  console.log('\n=== 仪表盘 ===');
  await evaluate(`location.hash = '#/dashboard'`);
  await waitFor(`document.getElementById('dash-check')?.innerText && !document.getElementById('dash-check').innerText.includes('读取中')`, { label: '仪表盘数据到达' });
  const dashCheck = await text('dash-check');
  const dashWindow = await text('dash-window');
  const dashSummary = await text('dash-summary');
  const countdown = await text('dash-countdown');
  const nextAt = await text('dash-next-at');
  const slotCount = await evaluate(`document.querySelectorAll('#dash-slots li').length`);
  const versionText = await text('dash-version');
  check(dashCheck.length > 0, '今日状态 chip 有文案', dashCheck);
  check(/21:00/.test(dashWindow), '窗口 chip 回显配置的 21:00', dashWindow);
  check(dashSummary.length > 0, '摘要文案非空', dashSummary);
  check(/\d{1,2}:\d{2}:\d{2}/.test(countdown), '倒计时为 HH:MM:SS', countdown);
  check(nextAt.length > 0 && !nextAt.includes('0001'), '下一档时刻不是零值', nextAt);
  check(slotCount >= 2, '时间轴渲染出 2 个档位', String(slotCount));
  check(/1\.0\.0/.test(versionText), '服务版本回显 1.0.0', versionText);
  await shot('01-dashboard');

  // 倒计时必须本地递减（不依赖额外请求）
  const before = await text('dash-countdown');
  // 2.6 秒：无头页面即使 bringToFront 也可能有 1s 级节流，等两个周期才稳。
  await new Promise((r) => setTimeout(r, 2600));
  const after = await text('dash-countdown');
  check(before !== after, '倒计时在本地每秒递减', `${before} -> ${after}`);

  // ---------- 操作按钮 ----------
  const hintBefore = await text('dash-action-hint');
  await evaluate(`document.getElementById('btn-probe').click()`);
  await new Promise((r) => setTimeout(r, 700));
  const hintAfter = await text('dash-action-hint');
  check(hintBefore !== hintAfter || hintAfter.length > 0, '定位预检有反馈文案', hintAfter);
  await shot('02-dashboard-probe');

  // ---------- 配置 ----------
  console.log('\n=== 配置 ===');
  await evaluate(`location.hash = '#/config'`);
  await waitFor(`!document.getElementById('view-config')?.hidden`, { label: '配置视图可见' });
  await waitFor(`(document.getElementById('cfg-window-start')?.value ?? '') !== ''`, { label: '配置表单填充' });
  const lon = await evaluate(`document.getElementById('cfg-lon').value`);
  const winStart = await evaluate(`document.getElementById('cfg-window-start').value`);
  const passType = await evaluate(`document.getElementById('cfg-password').type`);
  const passValue = await evaluate(`document.getElementById('cfg-password').value`);
  const timeRows = await evaluate(`document.querySelectorAll('#time-rows .row').length`);
  const cfgVersion = await text('config-version');
  check(lon.startsWith('119.'), '经度回显', lon);
  check(winStart === '21:00', '窗口起点回显 21:00', winStart);
  check(passType === 'password', '密码框为 password 类型', passType);
  check(passValue === '••••', '密码以哨兵回显（不泄露明文）', passValue);
  check(timeRows >= 2, '时刻多档渲染成 2 行', String(timeRows));
  check(cfgVersion.length > 0, '显示配置版本', cfgVersion);

  // ---------- 自绘控件（computed style） ----------
  console.log('\n=== 自绘控件（computed style）===');
  const appearanceOf = (id) =>
    evaluate(`getComputedStyle(document.getElementById(${JSON.stringify(id)})).appearance`);
  const appearance = {};
  for (const id of ['cfg-notify-type', 'cfg-vacation-notify', 'cfg-window-start', 'cfg-lon']) {
    const got = await appearanceOf(id);
    appearance[id] = got;
    check(got === 'none', `#${id} appearance === none`, got);
  }
  // number：实现选的是 appearance:textfield + 隐藏 spin button（两者等价于"看不到原生控件"）。
  const jitterAppearance = await appearanceOf('cfg-jitter');
  appearance['cfg-jitter'] = jitterAppearance;
  console.log(`  [INFO] appearance：${Object.entries(appearance).map(([k, v]) => `${k}=${v}`).join(' · ')}`);
  check(jitterAppearance === 'textfield' || jitterAppearance === 'none',
    '#cfg-jitter appearance 为 textfield/none（原生上下箭头已隐藏）', jitterAppearance);

  const cbRect = await rectOf('#cfg-vacation-notify');
  check(cbRect !== null && cbRect.width >= 16 && cbRect.width <= 22,
    '自绘复选框宽度 16–22px', cbRect ? `${cbRect.width.toFixed(2)}px` : 'null');
  if (cbRect) console.log(`  [INFO] #cfg-vacation-notify ${cbRect.width.toFixed(2)}×${cbRect.height.toFixed(2)}px`);

  // 无内联 style：CSP 是 style-src 'self'（无 'unsafe-inline'），任何 style 属性都会被丢弃。
  const styledCount = await evaluate(`document.querySelectorAll('[style]').length`);
  check(styledCount === 0, '配置视图渲染后全文档无内联 style 属性', `${styledCount} 处`);
  await shot('03-config');

  // 保存一次，验证 PUT 往返。等待条件必须盯"版本号变化"：toasts 里可能还留着
  // 登录/预检的旧消息，拿 toast 非空当完成信号会提前放行、误判为没保存。
  const verBeforeSave = await text('config-version');
  await evaluate(`document.getElementById('btn-config-save').click()`);
  await waitFor(
    `(document.getElementById('config-version')?.innerText ?? '') !== ${JSON.stringify(verBeforeSave)} || (document.getElementById('config-msg')?.innerText ?? '').length > 0`,
    { label: '保存完成（版本号变化或报错）', timeout: 15000 }
  );
  // 保存成功走 toast（config-msg 只承载错误），所以断言 toast + 版本号推进。
  const saveMsg = await text('toasts');
  const cfgMsg = await text('config-msg');
  const cfgVersionAfter = await text('config-version');
  if (cfgMsg) console.log(`  [INFO] config-msg: ${cfgMsg.replace(/\n/g, ' | ').slice(0, 300)}`);
  check(/保存|成功|v\d/i.test(saveMsg), '保存后有成功反馈', saveMsg.replace(/\n/g, ' | ').slice(0, 120));
  check(cfgVersionAfter !== cfgVersion || /v\d/.test(saveMsg), '配置版本已推进', `${cfgVersion} -> ${cfgVersionAfter}`);
  await shot('04-config-saved');

  // ---------- 浏览器定位（CDP 模拟，核心用例） ----------
  // 参考向量：WGS-84 119.20463,26.04920 → GCJ-02 119.209433,26.046112（§13 定位 v1.1）。
  console.log('\n=== 浏览器定位（CDP 模拟）===');
  await send('Browser.grantPermissions', { origin: BASE, permissions: ['geolocation'] });
  await T('Emulation.setGeolocationOverride', { latitude: 26.0492, longitude: 119.20463, accuracy: 15 });

  const verBeforeGeo = await text('config-version');
  const geoOk = await clickGeoSettle('定位成功并落定状态行');
  check(near(geoOk.lon, 119.209433, 0.000002), '#cfg-lon 回填为 GCJ-02 119.209433（±2e-6）', geoOk.lon);
  check(near(geoOk.lat, 26.046112, 0.000002), '#cfg-lat 回填为 GCJ-02 26.046112（±2e-6）', geoOk.lat);
  check(/GCJ-02/.test(geoOk.text), '#geo-status 说明已转换为 GCJ-02', geoOk.text);
  check(/\bok\b/.test(geoOk.cls), '#geo-status 为 ok 态', geoOk.cls);
  check(geoOk.version === verBeforeGeo, '定位只回填、不自动保存（配置版本未变）', `${verBeforeGeo} -> ${geoOk.version}`);
  await shot('verify-geo-ok');

  // 精度 > 100 m 时追加"精度较低"提示（§13 定位 v1.1）。
  await T('Emulation.setGeolocationOverride', { latitude: 26.0492, longitude: 119.20463, accuracy: 350 });
  const geoLow = await clickGeoSettle('低精度定位落定状态行');
  check(/精度较低/.test(geoLow.text), '精度 350 m 时提示「精度较低」', geoLow.text);

  // 收回权限后必须落到 err 分支：状态行有文案 + err 态（不能"点了没反应"）。
  await send('Browser.resetPermissions', {});
  const geoDenied = await clickGeoSettle('定位被拒后落定状态行');
  let denial = geoDenied;
  if (!/\berr\b/.test(denial.cls)) {
    // 部分 Chrome 版本里 resetPermissions 只把权限打回 "ask"，无头模式不会真的拒绝；
    // 这时用显式 denied 复现同一分支（断言不变，只换触发方式）。
    await send('Browser.setPermission', {
      origin: BASE,
      permission: { name: 'geolocation' },
      setting: 'denied',
    });
    denial = await clickGeoSettle('显式拒绝权限后落定状态行');
    console.log('  [INFO] resetPermissions 未触发拒绝分支，已改用 Browser.setPermission=denied 复现');
  }
  check(/\berr\b/.test(denial.cls), '#geo-status 为 err 态（权限被拒）', denial.cls);
  check(denial.text.length > 0, '拒绝定位时状态行有可执行文案', denial.text);
  await shot('verify-geo-err');
  // 复位权限，别把"被拒"状态带给后面的用例。
  try { await send('Browser.resetPermissions', {}); } catch { /* 忽略 */ }

  // ---------- 日志（SSE） ----------
  console.log('\n=== 日志 ===');
  await evaluate(`location.hash = '#/logs'`);
  await waitFor(`!document.getElementById('view-logs')?.hidden`, { label: '日志视图可见' });
  await sleep(2500);
  const conn = await text('logs-conn');
  const logLines = await evaluate(`document.querySelectorAll('#logs-list > *').length`);
  check(conn.length > 0, 'SSE 连接状态有文案', conn);
  check(/已连接|连接|connected/i.test(conn), 'SSE 显示已连接', conn);
  check(logLines > 0, '日志列表已渲染条目', String(logLines));
  await shot('05-logs');

  // ---------- 历史 ----------
  console.log('\n=== 历史 ===');
  await evaluate(`location.hash = '#/runs'`);
  await waitFor(`!document.getElementById('view-runs')?.hidden`, { label: '历史视图可见' });
  await sleep(600);
  const runsCount = await text('runs-count');
  const runRows = await evaluate(`document.querySelectorAll('#runs-list > *').length`);
  check(runsCount.length > 0, '历史计数有文案', runsCount);
  check(runRows > 0, '历史列表有运行记录（冒烟那次补跑）', String(runRows));
  await shot('06-runs');

  // 打开详情抽屉
  if (runRows > 0) {
    // 行结构是 <li><button class="run-item" data-id>…</button></li>，委托监听看 closest('.run-item')，
    // 所以必须点按钮本身而不是 li。
    await evaluate(`document.querySelector('#runs-list .run-item').click()`);
    await sleep(500);
    const drawerOpen = await evaluate(`!document.getElementById('drawer')?.hidden`);
    const drawerBody = await text('drawer-body');
    check(drawerOpen, '详情抽屉可打开');
    check(drawerBody.length > 20, '抽屉内有运行详情', drawerBody.slice(0, 40).replace(/\n/g, ' '));
    await shot('07-run-detail');
    // 详情渲染完再查一次内联 style：错误框是 CSSOM 写出来的（app.js:1525），
    // 静态模板查不到，只有这里能抓到。这是「无内联 style」契约的运行时一侧。
    const styledAfterDrawer = await evaluate(`document.querySelectorAll('[style]').length`);
    const styledSample = await evaluate(
      `(document.querySelector('[style]')?.outerHTML ?? '').replace(/\\s+/g, ' ').slice(0, 120)`);
    check(styledAfterDrawer === 0,
      '运行详情渲染后全文档仍无内联 style（否则命中 app.js:1525 的 CSSOM 写法）',
      `${styledAfterDrawer} 处：${styledSample}`);
    await evaluate(`document.getElementById('drawer-close').click()`);
  }

  // ---------- 移动端 ----------
  console.log('\n=== 移动端 390x844 ===');
  await setViewport(390, 844, true);
  await evaluate(`location.hash = '#/dashboard'`);
  await sleep(700);
  await evaluate(`window.scrollTo(0, 0)`);
  await sleep(300);
  const overflow = await evaluate(`document.documentElement.scrollWidth - document.documentElement.clientWidth`);
  check(overflow <= 1, '移动端无横向溢出', `overflow=${overflow}px`);

  const mobileAsidePos = await styleOf('aside.sidebar', 'position');
  check(mobileAsidePos === 'sticky' || mobileAsidePos === 'fixed',
    '移动端侧栏折成顶部条后仍是 sticky/fixed', mobileAsidePos);
  const mobileNav = await rectOf('#nav');
  const mobileMain = await rectOf('#main');
  if (mobileNav && mobileMain) {
    check(mobileNav.bottom <= mobileMain.top + 2,
      '移动端导航条在主内容之上（不重叠）',
      `nav.bottom=${mobileNav.bottom.toFixed(1)} main.top=${mobileMain.top.toFixed(1)}`);
    console.log(`  [INFO] 移动端 overflow=${overflow}px · aside.position=${mobileAsidePos} · ` +
      `nav.bottom=${mobileNav.bottom.toFixed(1)} · main.top=${mobileMain.top.toFixed(1)}`);
  } else {
    check(false, '移动端 #nav / #main 几何信息可取', JSON.stringify({ mobileNav, mobileMain }));
  }
  await shot('08-mobile-dashboard');
  await shot('verify-mobile-topbar');

  await setViewport(1440, 900);
  await evaluate(`location.hash = '#/config'`);
  await sleep(500);

  // ---------- 控制台干净 ----------
  console.log('\n=== 控制台 / CSP ===');
  check(exceptions.length === 0, '无未捕获 JS 异常', exceptions.join(' | ').slice(0, 200));
  check(consoleErrors.length === 0, '无 console.error', consoleErrors.join(' | ').slice(0, 200));

  // 页面内监听（securitypolicyviolation）+ Log 域的 security error 合并去重。
  const pageCsp = await evaluate(`(window.__cspViolations ?? [])`);
  const allCsp = Array.from(new Set([...cspViolations, ...(Array.isArray(pageCsp) ? pageCsp : [])]));
  check(allCsp.length === 0, '无 CSP 违规（含 Refused to）', allCsp.join(' | ').slice(0, 300));
} catch (err) {
  failures.push('脚本异常: ' + err.message);
  console.log(`  [FAIL] ${err.message}`);
  try { await shot('99-failure'); } catch { /* 忽略 */ }
} finally {
  ws.close();
}

function sleep(ms) { return new Promise((r) => setTimeout(r, ms)); }

console.log(`\n结果：${pass} 项通过，${failures.length} 项失败（Chrome ${version.Browser}）`);
if (failures.length) { failures.forEach((f) => console.log('  × ' + f)); process.exit(1); }
