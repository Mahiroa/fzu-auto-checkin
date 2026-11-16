# 不想复制粘贴？用控制台脚本把凭证直接交给面板

## 先说清楚：Cookie 拿不到，token 拿得到

统一身份认证下发的 `SESSION` 是 **HttpOnly** 的：

```
Set-Cookie: SESSION=0f1e2d3c-…; Path=/; Secure; HttpOnly
```

`HttpOnly` 意味着**任何页面脚本都读不到它**（`document.cookie` 里根本没有这一条），
只有 DevTools → 应用 → Cookie 面板能看见并复制。所以"运行一段脚本就自动取到 Cookie"
在浏览器里做不出来，这跟脚本怎么写无关。

但**晚点名的免登凭证（token）在地址栏里**：CAS 换取完成后浏览器会停在

```
https://yzsxg.fzu.edu.cn/plug-in/livecloud/project/fzu/attn/index.action?token=XXXXXXXX&contextPath=
```

这个页面的脚本是自己人（同源），`location.href` 随便读。所以自动化要自动化这一个。

---

## 第 1 步：先落到晚点名页面

地址栏粘这一条回车（已经登录过统一身份认证的话会直接跳过去）：

```
https://sso.fzu.edu.cn/login?service=https%3A%2F%2Fyzsxg.fzu.edu.cn%2Flivecloud%2Fproject%2Ffzu%2Fattn%2Foauth2%2Fcallback.action
```

落地后看一眼地址栏，应该能看到 `…/index.action?token=…`。

## 第 2 步：F12 → Console，粘这一段

### A. 现在就能用：复制到剪贴板

```js
(() => {
  const t = new URL(location.href).searchParams.get('token');
  if (!t) return console.error('当前页面地址里没有 token：先打开晚点名页面再来一次');
  const ta = document.createElement('textarea');
  ta.value = t; document.body.appendChild(ta); ta.select();
  const ok = document.execCommand('copy'); ta.remove();
  console.log(ok ? '%c免登凭证已复制到剪贴板，回面板粘进「免登凭证」保存即可' : '%c复制失败，手动选中这行：\n' + t,
              'color:#0a0;font-weight:bold');
})()
```

粘回面板 → 点「保存」。到这里已经不用去 DevTools 里翻 Cookie 了。

### B. 连粘贴都省掉：直接把凭证交给面板（v1.3.2 起）

```js
(() => {
  const t = new URL(location.href).searchParams.get('token');
  if (!t) return console.error('当前页面地址里没有 token：先打开晚点名页面再来一次');
  window.open('https://<你的面板域名>/#token=' + t, '_blank');
})()
```

新标签会打开面板，自动走：取走链接里的凭证（并立刻从地址栏抹掉）→ 校验 → 写入配置 →
状态行显示「免登凭证已自动保存 …」。**不需要点任何按钮。**

想更省事就做成书签（地址栏新建一个书签，网址填下面这一行），以后在晚点名页面点一下书签即可：

```
javascript:(()=>{const t=new URL(location.href).searchParams.get('token');if(!t)return alert('当前页面没有 token，请先打开晚点名页面');window.open('https://<你的面板域名>/#token='+t,'_blank')})()
```

> `<你的面板域名>` 换成你自己部署时用的域名（nginx 反代里的 `server_name` / 证书域名，例如 `checkin.example.com`）。
> 本仓库不写任何真实域名：面板入口一旦公开，唯一屏障只剩管理密码。

> 控制台第一次粘贴代码时，Chrome 可能要求先输入 `允许粘贴`（allow pasting），
> 这是浏览器对"粘贴到控制台"的防诈骗保护，不是本项目的限制。

---

## 为什么不用 Cookie 也能长期跑

面板有两条自愈路径：

1. token 过期 → 每轮签到前 `init` 失败会自动用**已保存的 Cookie 或学号密码**换新 token 并回写；
2. 两者都没配 → 日志会明确写「token 被拒绝，请填 Cookie 或账密」。

所以最省事的组合是：**学号密码**（一次输入，永久自动）> Cookie（一次粘贴，自动续期）> 手动 token（每次过期重新折腾）。
