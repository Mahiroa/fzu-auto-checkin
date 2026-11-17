# 智汇福大晚点名接口协议（逆向笔记）

> 来源：MuMu 模拟器 + adb logcat 抓取 H5 加载链路 + 前端 JS 静态分析。
> 未使用 Frida、未破解任何加密通信之外的内容——密钥本身硬编码在前端源码里。

## 1. 整体链路

```
智汇福大 App
  └─ 内嵌 WebView 加载晚点名 H5：
     https://yzsxg.fzu.edu.cn/plug-in/livecloud/project/fzu/attn/index.action
         ?token=<免登token>&contextPath=
  └─ 免登 token 由 App 侧经 CAS 链路换取：
     sso.fzu.edu.cn/login?service=...attn/oauth2/callback.action (CAS 标准)
       → authorize.action?token=xxx
       → callback.action?ticket=ST-xxx
       → index.action?token=xxx
```

**关键结论**：晚点名业务是普通 H5（无 SSL Pinning 问题），API 认证只靠一个
`token`（H5 URL 查询参数 → 请求头 `token`）。拿到 token 即可完全用 HTTP 复现。

## 2. API 清单（BASE = `https://yzsxg.fzu.edu.cn/livecloud/project/fzu/attn`）

| 接口 | 方法 | 参数 | 说明 |
|------|------|------|------|
| `init.action` | POST | `{}`（明文 JSON） | 今日计划/状态：`clockSuccess`、`clockWay`、`clockStartTime/EndTime`、`schoolPosition`（签到范围多边形）、`planId`、`campusList` 等 |
| `currentTimestamp.action` | GET | `param=`（加密 `{}`） | 服务器毫秒时间戳，用于迟到判定 |
| `check.action` | GET | `param=`（加密） | 定位是否在范围内，返回命中校区数组 |
| `clockIn.action` | GET | `param=`（加密） | 提交打卡，返回 `{attn: {...}}` |

响应统一格式：`{success: bool, code: int(1=成功), data: ..., msg: string}`。

**加密**（`Encrypt()`，前端源码硬编码）：

```
AES-128-CBC，KEY = IV = "apexinfoapexinfo"，PKCS7，输出 Base64
请求时：?param=encodeURIComponent(Encrypt(JSON对象))
```

**认证**：所有请求带 Header `token: <免登token>`。

## 3. 打卡参数（clockIn）

```jsonc
{
  "campus": "<check.action 返回的命中校区 id>",
  "lon": 119.20463,            // 高德 GCJ-02 经度
  "lat": 26.04920,             // 纬度
  "startTime": "21:30",        // 来自 init
  "endTime": "23:59",          // 来自 init
  "actualLocation": "标题 地址", // App 内用高德逆地理拼接，可自填
  "caIsNo": "0",               // H5 URL 参数 FCaIsNo，默认 "0"
  "collUnit": "<check 返回的第一个命中对象>",
  "way": 1,                    // clockWay：1=校内 2=校外 3=无需
  "isLate": 0                  // 按服务器时间与 endTime 比较，1=迟到
}
```

`check.action` 参数：

```jsonc
{
  "mapType": "amap",
  "longitude": 119.20463,
  "latitude": 26.04920,
  // 注意：这里要的是 init 里那份 **FPosition 整串**，形如
  //   {"range":[{...},{...}],"mapType":"amap"}
  // 不是它内部那个 range 数组摊平后的 [{"id":...},...]。
  "range": "{\"range\":[...],\"mapType\":\"amap\"}"
}
```

> ⚠️ **2026-11-17 线上实测的坑**：同一个 token、同一个坐标，只改 `range` 形状——
> 发多边形数组 → `{"success":false,"code":0,"msg":"打卡地点校验发生异常"}`（HTTP 仍 200）；
> 发 FPosition 对象串 → `{"success":true,"code":1,"data":[{...旗山校区...}]}`。
> Go 版在 1.3.2 之前把数组当 range 发，因此每次都在 check 阶段被拒；Python 版
> （`src/checkin.py` 把 `{"range": polygons}` 整体 dumps）一直是对象形状，才没踩到。

## 4. 获取 token

token 由 App 登录后下发，获取方式按成本排序：

1. **Android 真机/模拟器 + adb**（已验证，最简单）：
   ```bash
   adb logcat -d | grep -oE "index\.action\?token=[A-Za-z0-9]+" | tail -1
   ```
   前提：手机上打开过一次晚点名页面。token 有效期未知，失效重取即可。

2. **CAS 自动登录**（TODO，`src/login.py`）：
   走 `sso.fzu.edu.cn` 统一身份认证（即智汇福大 App 登录用的学号+密码），
   登录成功 → ticket → callback 换 token。
   实现后无需手动获取 token，适合 GitHub Actions 云端长期运行。
   注意：sso.fzu.edu.cn 是新版前端（登录表单 JS 动态渲染 + clientredirect 多端适配），
   需继续分析其前端 JS 的密码加密与提交接口，工作量中等。

3. iOS 用户：无越狱抓不到 App 内 token（SSL Pinning）。建议等 CAS 方案，
   或借用任意一台 Android 设备执行方式 1 一次。

## 5. 免责声明

- 本协议笔记仅供学习交流，禁止用于代挂、有偿服务或伪造到岗记录。
- 定位参数应使用使用者的**真实位置**；脚本不内置、不伪造任何坐标。
