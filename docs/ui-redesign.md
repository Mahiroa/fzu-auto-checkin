# 管理界面重设计契约（UI v2）

这一份是**执行契约**，不是设计稿。所有人按它改，改完按 §7 自检。
文件名、id、选择器里出现的字面量都当契约对待：**逐字**。

---

## 0. 任务与分工

| 范围 | 文件 | 负责人 |
| --- | --- | --- |
| 主界面样式（整份重做） | `server/web/static/style.css` | css-owner |
| 主界面结构与文案 + 主题切换 | `server/web/static/index.html`、`server/web/static/app.js`（只在**文件末尾追加**一段） | html-owner |
| 两张独立静态页 | `web/configurator.html`、`web/promo.html` | pages-owner |

前端是零构建的原生 HTML/CSS/ES module，改完 `go build` 即生效，没有打包步骤。

---

## 1. 硬门禁（违反 = 构建红）

门禁来自 `server/internal/server/zz_layout_review_test.go` 与 `zz_review_test.go`，静态扫描字面量。

### 1.1 体积（未压缩字节）

| 文件 | 上限 | 现状 |
| --- | --- | --- |
| `index.html` | 40960 B | 31274 B |
| `style.css` | 45056 B（**目标 ≤40000 B**） | 36520 B |
| `app.js` | 98304 B | 81827 B |
| 三文件合计 | **163840 B** | 149621 B |

合计只剩 14 KB 余量。美化靠**替换**不靠**堆加**：新增规则前先删旧规则。

### 1.2 `index.html` 的 CSP 与资源

- `<script` 全文恰好 **1** 处，且逐字是 `<script type="module" src="./app.js">`
- 禁止 ` style="`、禁止 `<style`、禁止 ` on*=`（onclick/oninput/... 全部）、禁止 `javascript:`
- 禁止任何 `http://` `https://` 字样（含注释）
- 禁止 `//cdn`、`unpkg`、`jsdelivr`、`googleapis`、`@import`

### 1.3 `index.html` 的结构（逐字匹配）

- `class="sidebar"` 必须存在，侧栏必须是 `<aside>`
- **`<aside class="sidebar">` 这 23 个字符必须逐字出现且只出现一次**——不能加 `aria-label` 或其他属性，测试用这个字面量切分 aside 内部
- `id="shell"` 在 `<aside` 之前；`id="nav"` 在 `id="main"` 之前
- `<aside class="sidebar">` 到 `</aside>` 之间必须有 `id="btn-logout"` 和 `side-version`
- 必须存在的 id：`nav`、`main`、`btn-geo`、`geo-status`
- `data-route="#/dashboard"`、`#/config`、`#/logs`、`#/runs` 各 **恰好 1** 处
- `class="nav-btn"` 全文恰好 **4** 处，且 4 个都在 `<nav id="nav">` 与 `</nav>` 之间
- **§附录里的 152 个 id 一个都不能少**（基线在 `docs/ui-baseline-ids.txt`）

### 1.4 `style.css` 的字面要求

逐字出现：

- `--side-w: 248px`
- `grid-template-columns: var(--side-w) minmax(0, 1fr)`（`#shell` 两列）
- `aside.sidebar` 单独一条规则，含 `position: sticky` 或 `position: fixed`
- `@media (max-width: 860px)` 块内必须含 `position: sticky` 与 `grid-template-columns: minmax(0, 1fr)`；该媒体块**最后一条是普通规则**（测试按 `}\n}\n` 切块）
- `input[type=time] { background-image: var(--ico-clock)`（`{` 后紧跟这条声明）
- `input[type=date] { background-image: var(--ico-calendar)`
- 一条共用输入规则，选择器列表同时含 `input[type=text]`、`input[type=password]`、`input[type=number]`、`input[type=time]`、`input[type=date]`、`select`、`textarea`，且该规则含 `appearance: none`
- `input[type=checkbox] {` 独占规则（写成 `input[type=checkbox] {`，不许并进选择器列表），含 `appearance: none`；另需 `input[type=checkbox]::after`
- `input[type=number] {` 独占规则，含 `appearance: textfield` 或 `appearance: none`
- `input[type=number]::-webkit-inner-spin-button` 与 `input[type=number]::-webkit-outer-spin-button` 所在规则含 `appearance: none`
- `::-webkit-calendar-picker-indicator` 所在规则含 `opacity: 0`
- `appearance: none` 至少 3 处；`data:image/svg+xml` 至少 3 处；`::-webkit-scrollbar` 存在；`:focus-visible` 存在

禁止：

- `outline: none`（一律用 `outline: 2px solid transparent` + `box-shadow` 表达焦点环）
- `@import`、`url(http`、`url(//`、`url("http`
- data URI 里裸 `#`（必须 `%23`）；SVG 命名空间只许写成 `xmlns='http://www.w3.org/2000/svg'`（单引号，测试会摘掉这串）

### 1.5 `app.js`

- 不许出现 `.style.`、`.innerHTML`、`new Function(`、`eval(`
- 必须保留：`import('./map-picker.js')`、`navigator.geolocation.getCurrentPosition`、`wgs84ToGcj02(`、`GCJ-02`
- 本次只允许在**文件末尾追加**主题切换片段，其余一行不改

---

## 2. 视觉方向

一句话：**像一台安静的打卡终端，不像一个 SaaS 产品页。**

- 中性色打底，只有一个强调色；强调色只在"当前状态 / 主操作 / 焦点"出现，不作装饰铺满。
- 层次靠 **1px 细边框 + 极浅阴影**，不用厚投影和大面积渐变；暗色下允许一层 260px 半径的极淡径向光（≤6% 不透明度）铺在页面顶部。
- 圆角统一 12–16px（卡片 14px，控件 10px，小徽标 999px）。不用 4px 老派圆角，也不用 24px 泡泡感。
- 字体只用系统栈，不引外部字体。标题收紧字距（`letter-spacing: -.01em`），数字用 `font-variant-numeric: tabular-nums`。
- 动效只做 120–180ms 的 `background/border/transform` 过渡，`prefers-reduced-motion: reduce` 时全关。不做入场动画、不做悬浮跳动。
- 中文排版：正文 14px/1.65，说明文字 12.5px，标签 12px；行宽不超过 68 字符。

### 2.1 令牌（精确值，直接抄）

暗色（默认）：

```
--bg: #0d0f12          /* 页面底 */
--bg-2: #090b0d        /* 内凹区（日志、代码） */
--surface: #14171c     /* 卡片 */
--surface-2: #1a1e24   /* 卡片内的次级块 / hover */
--line: #262b33        /* 主分隔线 */
--line-2: #1e232a      /* 更弱的线 */
--fg: #e7eaee
--fg-2: #9aa4b0        /* 次要文字 */
--fg-3: #6c7681        /* 占位、禁用的辅助 */
--accent: #7aa2f7      /* 主强调（柔和蓝） */
--accent-fg: #0d0f12   /* 强调面上的字 */
--accent-soft: color-mix(in srgb, var(--accent) 16%, transparent)
--ok: #5fbf8a
--warn: #d9a960
--err: #e06c75
--ring: color-mix(in srgb, var(--accent) 55%, transparent)
```

亮色（`prefers-color-scheme: light` 或 `[data-theme="light"]`）：

```
--bg: #f6f7f9
--bg-2: #eef0f4
--surface: #ffffff
--surface-2: #f7f8fa
--line: #e2e5ea
--line-2: #eceef2
--fg: #171b21
--fg-2: #5b6673
--fg-3: #8b95a1
--accent: #2f6fd0
--accent-fg: #ffffff
--ok: #1f7a4d
--warn: #8a6516
--err: #c0392b
```

尺寸令牌：`--r-card: 14px`、`--r-ctrl: 10px`、`--r-sm: 8px`、`--ctrl-h: 38px`、`--side-w: 248px`、`--gap: 14px`。

对比度：正文 ≥ 7:1，次要文字 ≥ 4.5:1，`--accent` 上放 `--accent-fg` 时 ≥ 4.5:1（暗色主题 `--accent` 偏亮，所以强调面上的字用深色）。

---

## 3. 结构增强（html-owner 照抄，css-owner 照此写样式）

新类名**只允许**用下面列出的这些。css-owner 必须给每个都写样式，html-owner 不许发明别的类名。

### 3.1 侧栏

```html
  <aside class="sidebar">
    <div class="brand">
      <img src="./favicon.svg" alt="" width="22" height="22" decoding="async">
      <span class="brand-text">
        <b class="brand-name">福大晚点名</b>
        <span class="brand-sub">自动签到</span>
      </span>
    </div>
    <nav id="nav" aria-label="主导航">
      <button class="nav-btn" type="button" data-route="#/dashboard">
        <svg class="nav-ico" viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d="M4 4h7v7H4zM13 4h7v4h-7zM13 10h7v10h-7zM4 13h7v7H4z"/></svg>
        <span>仪表盘</span>
      </button>
      <button class="nav-btn" type="button" data-route="#/config">… 齿轮图标 …<span>配置</span></button>
      <button class="nav-btn" type="button" data-route="#/logs">… 终端图标 …<span>日志</span></button>
      <button class="nav-btn" type="button" data-route="#/runs">… 时钟图标 …<span>历史</span></button>
    </nav>
    <div class="sidebar-foot">
      <button id="btn-theme" class="ghost theme-btn" type="button">跟随系统</button>
      <button id="btn-logout" class="ghost" type="button">退出登录</button>
      <span class="side-version" id="side-version">版本 —</span>
    </div>
  </aside>
```

图标用 `fill="currentColor"` 的 24×24 path，`aria-hidden="true"`。导航项结构 = `图标 + <span>文字</span>`，`class="nav-btn"` 保持在 button 上。

### 3.2 内容区页头

四个视图统一用：

```html
<div class="view-head">
  <div class="view-head-main">
    <h2>仪表盘</h2>
    <p class="view-sub">今天打没打、下次几点、最近一次结果</p>
  </div>
  <div class="view-head-actions">…原有控件…</div>
</div>
```

仪表盘原来那行 `<h2 class="vh">仪表盘</h2>` 换成上面的页头（不要既留 vh 标题又加页头，重复标题会被读屏念两遍）。

### 3.3 卡片

```html
<article class="card">
  <div class="card-head">
    <h3 class="card-title">今日考勤计划</h3>
    <span class="card-note">按学校当天开放的时间段算</span>
  </div>
  …
</article>
```

`.card-head` 用 flex 两端对齐、`border-bottom: 1px solid var(--line-2)` 下方留 12px。原来"<h3>标题 <span class="muted small">补充</span></h3>"的写法统一换成 `.card-head`。

### 3.4 仪表盘 KPI

第一张卡里的 `<dl class="kv">` 保留类名 `kv`（app.js 会给别的元素设这个类），追加 `kv-kpi`：

```html
<dl class="kv kv-kpi">
  <div><dt>今日</dt><dd id="dash-date">—</dd></div>
  <div><dt>上次成功</dt><dd id="dash-last-success">—</dd></div>
  <div><dt>最近决策</dt><dd id="dash-decision">—</dd></div>
  <div><dt>调度器</dt><dd id="dash-scheduler">—</dd></div>
  <div><dt>服务</dt><dd id="dash-version">—</dd></div>
</dl>
```

`.kv-kpi` 是 `grid-template-columns: repeat(auto-fit, minmax(140px, 1fr))`，每格左边 2px 竖线。

### 3.5 状态点与胶囊

```html
<span id="dash-check" class="chip"><i class="dot"></i>读取中…</span>
```

`.chip` 是胶囊（`border-radius: 999px`，1px 边线，`padding: 3px 10px`，12px 字）。`.dot` 是 6px 圆点，`background: currentColor`。app.js 会把 `class` 重设成 `chip` / `chip ok` / `chip warn` / `chip err`（见 §6.2），所以 `.chip.ok`、`.chip.warn`、`.chip.err` 三种状态色必须写。

### 3.6 空状态

`.empty` 保持纯文字节点（app.js 会改它的 `hidden`），CSS 用 `::before` 加一个 1px 虚线圆或短竖线做视觉锚点，不加文字图标。

### 3.7 配置页的分组

每个 `<fieldset class="card">` 的 `<legend>` 改成：

```html
<legend><span class="legend-text">账号</span><span class="legend-note">登录方式三选一</span></legend>
```

`.legend-note` 是 12px、`--fg-3`、`font-weight: 400`。

### 3.8 取消的东西

- 不再有"卡片右上角一段全角括号说明"；说明一律进 `.card-note` 或 `.legend-note`。
- 不用 emoji、不用装饰性图标字。
- 不加侧栏折叠、不加主题色选择器、不加动画开关。

---

## 4. 文案准则（去 AI 味，最重要的一节）

### 4.1 写什么

1. **说事实和代价**。"改完要重启才生效"比"配置将自动同步"有用。
2. **一句话一件事**。超过 40 字的说明拆两句。
3. **用动词开头**。按钮写"立即签到""测试登录""保存"，不写"提交""确定""开始使用"。
4. **具体数字**。写 `200 米`、`23:59:59`、`8 次`，不写"若干""适当"。
5. **允许口语**。"填哪个都行""顺手点一下""跑一次看看"。
6. **说明里可以承认边界**。"窗口外不会打卡"比"严格保证合规"可信。

### 4.2 不许出现

- 空话形容词：丝滑、极致、强大、智能、全新、一站式、无缝、贴心、优雅
- 套话连接：让我们、接下来、总而言之、值得注意的是、需要注意的是、值得一提的是
- 营销腔：赋能、打造、助力、开启旅程、立即体验、你值得拥有
- 客服腔：亲、小伙伴、如有疑问请随时联系、温馨提示、希望对你有帮助
- 结构腔：首先/其次/最后 三段式；"不仅仅是 X，更是 Y"
- emoji（含 ✅ ❌ ✨ 🚀 👉 📋 😵）与全角感叹号连用
- 英文破折号 `--` 当装饰；中文用 `——` 且每屏最多一处
- 括号里塞解释的双重嵌套（`（也就是（默认））` 这种）

### 4.3 必改清单（现状 → 目标语气）

| 位置 | 现状 | 改成 |
| --- | --- | --- |
| 账号 legend 提示 | "三种登录方式任填一种：Cookie（不用存密码）、「学号 + 密码」、免登凭证。都填时由服务端定优先级…" | "三种登录方式，填一种就够：Cookie、学号密码、免登凭证。都填了按服务端的优先级走，点「测试登录」能看出实际用了哪条。凭证过期时用学号密码自动续。" |
| 位置 legend 提示 | "填你在校区里常待位置的真实经纬度（和高德地图上看到的是同一套坐标）…" | "填你平时待的地方，经纬度跟高德上看到的一致就行。位置抖动只是在这个点附近小范围漂移，不会搬到别处。" |
| 地图入口说明 | "底图直接铺高德瓦片，不用注册、不用 key、不限次数…" | "不知道数字就点「在地图上选点」：底图是高德瓦片，不用注册、不用 key，拖到宿舍楼点一下就行。点出来就是高德那套坐标（GCJ-02），跟学校判定用的一致。" |
| 抖动说明 | "以你的常用位置为圆心，在这个半径范围内随机取一个点上报，看起来更像人在附近走动…" | "以常用位置为圆心随机取点，看起来像人在附近走动。填 0 就每次报同一个点；留空或填负数按 200 米算。" |
| 窗口说明 | "北京时间。开始时间和结束时间当天都算在窗口内；结束时间写成 23:59 时会自动算到 23:59:59。窗口不能跨过半夜…" | "北京时间，两头都算在窗口内。写 23:59 会按 23:59:59 算。窗口不能跨半夜，结束时间必须晚于开始。" |
| 浮动说明 | "加了浮动以后，算出来的时刻要是跑出窗口，会重新抽（最多 8 次）；还是出界就贴到窗口边上…" | "浮动后算出窗口外的时刻会重抽，最多 8 次；还是出界就贴到窗口边上，打卡前再核一次。所以浮动再大也不会在窗口外打卡。早到最多等多久：比目标时刻早到时最多等这么多分钟，等不到这档就不打。晚多少分钟还算准点：超过就算迟到，但照样会签。" |
| 推送说明 | "成功/越界/失败必推；已签到、跳过、假期默认静默。推送失败不影响签到结论。" | 保留，只把 `/` 改成顿号："成功、越界、失败必推；已签到、跳过、假期默认静默。推送失败不影响签到结论。" |
| 导出说明 | "保存前会先把旧配置备份成 config.json.bak；上面的密码…都按**明文**显示…" | 拆两句，去掉"上面的"指代："保存前会把旧配置备份成 config.json.bak。密码、免登凭证、Cookie、推送密钥都按明文回填，方便核对；只有「导出」下载的文件会自动脱敏成 ••••。" |
| 服务端说明 | "访问地址和数据存放目录改完要重启服务才会生效。上面那一项只有在你把管理页面挂到别的网站上时才需要填，平时留空就好。" | "改地址和数据目录要重启才生效。允许外部调用那项平时留空，只有把管理页挂到别的站上才填。" |
| 页脚 | "零构建 · 零依赖 · 数据都在本机 data/ 目录" | 保留（够人话），分隔符改成 `/` |
| 登录子标题 | "管理界面" | "本机管理界面" |
| 导入说明 | "这里只给从旧版配置搬过来用：把旧配置文件的全部内容…框里的示例是旧文件的写法，照着原样粘就行，不用改。" | "从旧版搬配置用这个：把旧 config.yaml 整段粘进来，先点「预览」对一遍——这步不会保存。确认无误再点「应用并保存」。" |

### 4.4 硬性限制

文案不能碰 §1.3 的结构字面量；`#/dashboard` 等路由、id、`data-*` 的名字一律不许翻译或改写。

---

## 5. 主题切换（html-owner）

`#btn-theme` 三态：跟随系统 → 浅色 → 深色 → 跟随系统。文案固定为 `跟随系统` / `浅色` / `深色`，`aria-live="polite"`。

app.js **末尾追加**（`main()` 之后、文件最后），独立 IIFE，不改其它代码：

```js
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
```

CSS 侧要同时支持：

- `@media (prefers-color-scheme: light)` 的默认浅色
- `:root[data-theme="light"]` 强制浅色（要覆盖系统暗色）
- `:root[data-theme="dark"]` 强制深色（要覆盖系统浅色）

写法建议（省字节）：

```css
:root, :root[data-theme="dark"] { /* 暗色令牌 */ }
@media (prefers-color-scheme: light) { :root:not([data-theme="dark"]) { /* 浅色令牌 */ } }
:root[data-theme="light"] { /* 浅色令牌，同一套值再写一次 */ }
```

---

## 6. app.js 会动的 DOM（不许改结构）

### 6.1 选择器依赖

`.nav-btn`（4 个，读 `dataset.route`）、`#pool-rows .row` 内的 `.r-lon/.r-lat/.r-actual/.r-weight/.r-pick`、`.t-time`、`.v-name/.v-start/.v-end`、`.has-secret`（内部第一个 `input` 或 `textarea` + `.mask-badge`）、`.clear-secret`、`[data-notify]`、`.run-item`。这些类名和嵌套关系**一个字都不能动**。

### 6.2 动态设置的 class（CSS 必须覆盖）

`chip`、`chip ok|warn|err`、`err`、`err-box`、`err-box-gap`、`hint`、`login-test-line`、`kv`、`log`、`log lv-debug|info|warn|error`、`ln-lv`、`ln-msg`、`ln-ts`、`mono`、`mono small`、`muted`、`muted small`、`run-item`、`spinner`、`st-ok|st-warn|st-err|st-idle`（运行状态，形如 `tag st-ok`）、`tag`、`times`、`toast`、`toast ok|warn|err`、`ri-when`、`ri-lat`。

### 6.3 map-picker.js 的类

`map-picker.js` 会生成 `.mp-*` 系列的节点（见该文件里的 `class` 字面量），`style.css` 里现有的 `.mp-*` 规则必须保留并一起重做。

---

## 7. 交付与自检

每个人改完**必须**自己跑一遍，红了就自己修：

```powershell
cd <仓库根>
# 1) 体积与字面门禁（最快的一条）—— go.mod 在 server/，测试必须在那个目录里跑
cd server
go test ./internal/server -run 'TestReviewLayout|TestReviewFrontendAndCSPContract' -count=1
# 2) 编译
go build -o fzucheckin.exe .\cmd\fzucheckin
cd ..
# 3) id 完整性（html-owner，见下）
```

上面第 2 步生成的 `server\fzucheckin.exe` 与第 3 步的 id 自查都按仓库根为基准；`go build` 本身在 `server/` 里执行。

id 基线自查（html-owner）：

```powershell
$now = ([regex]::Matches((Get-Content -Raw server\web\static\index.html), 'id="([^"]+)"') | ForEach-Object { $_.Groups[1].Value })
$base = Get-Content docs\ui-baseline-ids.txt
Compare-Object $base $now   # 必须无输出
```

报告格式（回给我）：改了什么、体积前后、门禁结果、还有哪些没覆盖到。
