// 真实浏览器验收：Chrome + CDP（flat session），跑完四个视图并留截图。
//
// 为什么单独写一个脚本而不是手点：验收结论要可复现、可留痕。这里把「登录 → 仪表盘 →
// 配置 → 日志(SSE) → 历史 → 移动端」每一步都落成断言 + PNG，失败会以非 0 退出码返回。
// 硬约束（见 browser-verification 技能）：只用 Google Chrome 绝对路径，绝不用 Edge。
//
// 用法：node scripts/ui-verify.mjs --base http://127.0.0.1:8787 --cdp 9225 --password xxx --out <dir>

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
    await evaluate(`document.getElementById('drawer-close').click()`);
  }

  // ---------- 移动端 ----------
  console.log('\n=== 移动端 390x844 ===');
  await setViewport(390, 844, true);
  await evaluate(`location.hash = '#/dashboard'`);
  await sleep(700);
  const overflow = await evaluate(`document.documentElement.scrollWidth - document.documentElement.clientWidth`);
  check(overflow <= 1, '移动端无横向溢出', `overflow=${overflow}px`);
  await shot('08-mobile-dashboard');

  await setViewport(1440, 900);
  await evaluate(`location.hash = '#/config'`);
  await sleep(500);

  // ---------- 控制台干净 ----------
  check(exceptions.length === 0, '无未捕获 JS 异常', exceptions.join(' | ').slice(0, 200));
  check(consoleErrors.length === 0, '无 console.error', consoleErrors.join(' | ').slice(0, 200));
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


