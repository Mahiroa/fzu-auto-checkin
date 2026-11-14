"""入口：初始化客户端 → 查询状态 → 定位校验 → 打卡 → 推送结果。

v1.0 主路径：直接使用 App 免登 token（获取方式见 docs/protocol.md），
无需在脚本里保存学号密码。CAS 自动登录为可选增强（src/login.py）。

"几点签"不再写在本文件里：窗口、自定义时刻、时间抖动、早到等待全部由 src/schedule
的 parse_schedule_config / resolve_run 决定，这里只把 Decision 翻译成
"签 / 等一会儿 / 跳过"三个分支，业务链路（假期、登录、查询、打卡、推送）保持原样。
"""
from src.config import load_config
from src.checkin import AttnClient, query_today_task, do_checkin
from src.notify import notify
from src.vacation import matched_range, today_cn
from src.schedule import (
    Decision,
    in_window,
    now_cn,
    parse_schedule_config,
    plan_slots,
    resolve_run,
    verify_in_window,
)

import datetime
import os
import re
import sys
import time

import requests


def _safe_stdout():
    """让 Windows 的 GBK 控制台不会因为 ✅ / ⚠ 之类的字符直接崩掉。

    GitHub Actions 是 UTF-8，这里不起作用；但本地 `python main.py` 时 cp936 控制台遇到
    无法编码的字符会抛 UnicodeEncodeError，整次签到在打印日志这一步就结束了——
    比签到本身失败更难排查，所以只放宽编码错误的处理（replace），不改输出内容。
    """
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(errors="replace")
        except Exception:
            pass


def _save_token(cfg, token: str):
    """把新 token 回写到 config.yaml（避免每次运行都重新登录）。"""
    try:
        path = cfg.get("_config_path")
        if not path:
            return
        text = open(path, encoding="utf-8").read()
        new_text, n = re.subn(
            r'(token:\s*")([^"]*)(")', rf"\g<1>{token}\g<3>", text, count=1
        )
        if n:
            open(path, "w", encoding="utf-8").write(new_text)
            print("新 token 已回写 config.yaml")
    except Exception as e:
        print(f"token 回写失败（不影响本次运行）: {e}")


# ============ 时间决策 ============

def _force_enabled() -> bool:
    """CHECKIN_FORCE=1 表示"手动补签"（workflow_dispatch 的 force 输入透传成该变量）。

    只认严格的 "1"：CI 里表达式可能落成空串，其它值一律走正常调度分支，
    免得一个手滑的 CHECKIN_FORCE=true 让定时任务整晚都跳过等待。
    """
    return (os.environ.get("CHECKIN_FORCE") or "").strip() == "1"


def _window_text(sched: dict) -> str:
    """窗口文案：日志里必须能直接看到"这一次允许 21:00-23:59 之间"。

    窗口来自配置，用户改过窗口后再看十几行日志就不会误以为还是默认时段。
    """
    return f"{sched['window_start']:%H:%M}-{sched['window_end']:%H:%M}"


def _target_text(decision: Decision) -> str:
    """目标时刻的展示文案：把"配的几点"和"抖动后实际按几点"一起说清。

    推送/日志里只写一个 21:41 会让人怀疑配置没生效；带上配置时刻与钳制标记，
    事后不用回头复算随机数就知道这一枪是哪一档、有没有被窗口边界拦下。
    """
    if decision.target is None:
        return "（无目标时刻）"
    text = f"{decision.target:%H:%M:%S}"
    slot = decision.slot
    if slot is not None and slot.configured is not None and slot.jittered:
        text += f"（配置 {slot.configured:%H:%M} + 时间抖动）"
    if slot is not None and slot.clamped:
        text += "（抖动越界，已钳制回窗口边界）"
    return text


def decide(cfg: dict, now: datetime.datetime = None, *, sched: dict = None, rng=None,
           slots=None) -> Decision:
    """把"现在签 / 等一会儿 / 跳过"收敛成一个不联网、不 sleep 的纯决策。

    为什么单独抽出来：这段判断一旦留在 main() 里，就只能靠真跑 GitHub Actions 来验证，
    而漏签恰恰是低频、难复现的故障。这里把配置、当前时间、随机源全部做成入参，
    任意时刻都能在本地断言分支走向（见自检脚本里的三个假 config）。

    slots 必须与首次决策用同一批：等待前后各抽一次时间抖动，会让"刚等到 21:41"变成
    "下一个目标是 21:56，等不起，跳过"，本次 cron 白等一趟。

    CHECKIN_FORCE=1 也在这里改判：拒绝等待、拒绝窗口外跳过，但仍把调度器原本的结论
    带在 reason 前缀 "force:" 后面，保证日志能看出"本来会怎么判"。
    """
    if now is None:
        now = now_cn()
    if sched is None:
        sched = parse_schedule_config(cfg)

    decision = resolve_run(sched, now, rng=rng, slots=slots)
    if not _force_enabled() or decision.mode == "run":
        return decision

    # 手动补签：等不起（人要立刻看到结果），也不能因为窗口外就放弃（正是补签场景）。
    # 窗口越界不放行在这一层，而是留给执行前的 verify_in_window 打印警告后放行，
    # 让"校验"始终只有一条实现路径。
    target = decision.target if decision.target is not None else now
    return Decision("run", decision.slot, target, 0.0, f"force:{decision.reason}")


def _skip_message(decision: Decision, now: datetime.datetime, sched: dict) -> str:
    """skip 分支的中文原因：说清"为什么今晚这一枪不打"，而不是只留一行 return。

    skip 一律静默（不登录、不推送）——延迟触发的补跑如果也推送，用户一晚会收到
    好几条互相矛盾的通知。
    """
    if decision.reason == "out_of_window":
        return (
            f"北京时间 {now:%H:%M:%S} 不在签到窗口 {_window_text(sched)} 内："
            "判定为延迟触发/跨日的补跑，静默跳过（不登录、不推送）。"
        )
    if decision.reason == "wait_too_long":
        gap = "未知"
        if decision.target is not None:
            gap = f"{int(round((decision.target - now).total_seconds() / 60.0))} 分钟"
        return (
            f"最近的签到时刻 {_target_text(decision)} 距今还有 {gap}，"
            "超过 max_wait_minutes 上限：交给后续 cron 档处理，本次静默跳过（不推送）。"
        )
    return f"调度器判定跳过（{decision.reason}），本次静默结束（不推送）。"


# ============ 主流程 ============

def main():
    _safe_stdout()
    cfg = load_config()

    # 时间判断全部交给调度器，主流程不再自己比较时刻（旧版硬编码的 21:30/21:35/23:59
    # 与"窗口外静默跳过 + 21:35 边缘等待"已整体删除，语义由 resolve_run 接管）
    sched = parse_schedule_config(cfg)
    now = now_cn()
    force = _force_enabled()
    if force:
        # 手动触发也要留下"当时几点、窗口是多少"，否则事后无法判断是补签还是准点签
        inside = in_window(now, sched["window_start"], sched["window_end"])
        print(
            f"CHECKIN_FORCE=1：当前北京时间 {now:%Y-%m-%d %H:%M:%S}，"
            f"签到窗口 {_window_text(sched)}（{'窗口内' if inside else '窗口外'}）。"
        )
        if not inside:
            print("[警告] 手动强制补签（窗口外）：当前不在窗口内，仍按 CHECKIN_FORCE=1 继续尝试签到。")

    # 一次规划、全程复用这批目标时刻：等待前后若各抽一次时间抖动，就可能出现
    # "刚等到 21:41，二次决策又算出 21:56 等不起 → 跳过"，本次 cron 白等一趟。
    slots = plan_slots(sched, now)
    decision = decide(cfg, now, sched=sched, slots=slots)
    if decision.reason.startswith("force:"):
        print(
            f"CHECKIN_FORCE=1：调度器原结论为 {decision.reason.split(':', 1)[1]}，"
            f"跳过等待与窗口外跳过，立即尝试签到（目标 {_target_text(decision)}）。"
        )

    if decision.mode == "skip":
        print(_skip_message(decision, now, sched))
        return

    if decision.mode == "wait":
        # 最多补睡 3 轮：sleep 只保证"不早于"，runner 时钟漂移或提前唤醒时二次决策
        # 会又给出一个极短的 wait，此时直接放弃等同于漏签。
        for _ in range(3):
            wait_seconds = max(decision.wait_seconds, 0.0)
            print(
                f"北京时间 {now_cn():%H:%M:%S} 距目标签到时刻 {_target_text(decision)} "
                f"还有 {int(wait_seconds)} 秒，等待后执行。"
            )
            # +1 秒余量：不加的话掐点醒来会比目标早几毫秒，二次决策仍是 wait。
            time.sleep(wait_seconds + 1)
            decision = decide(cfg, now_cn(), sched=sched, slots=slots)
            if decision.mode == "run":
                break
            if decision.mode == "wait" and decision.wait_seconds <= 120:
                continue
            print(
                f"等待结束后重新决策为 {decision.mode}（{decision.reason}），"
                "按跳过处理（不推送）。"
            )
            return
        else:
            print(
                f"连续 3 轮等待后仍未到执行点（{decision.reason}），按跳过处理（不推送）。"
            )
            return

    # 执行前二次校验：抖动与等待都可能把执行点推过窗口边界，这里保持唯一判定路径；
    # 手动补签是唯一例外——窗口外只警告不拦截，否则 force 就失去意义。
    started = now_cn()
    try:
        verify_in_window(started, sched["window_start"], sched["window_end"])
    except ValueError as e:
        if not force:
            print(f"{e}，放弃本次执行（不推送）。")
            return
        print(
            f"[警告] 手动强制补签（窗口外）：{e}；当前北京时间 {started:%Y-%m-%d %H:%M:%S}，"
            f"窗口 {_window_text(sched)}，按 CHECKIN_FORCE=1 继续尝试签到。"
        )

    exec_text = f"{started:%H:%M:%S}"

    # 假期自动跳过：命中校历/自定义区间时什么都不做（默认静默）
    vac = matched_range(cfg)
    if vac:
        print(f"假期中（{vac}），跳过本次签到。今天是 {today_cn()}。")
        if (cfg.get("vacation") or {}).get("notify"):
            notify(cfg, f"智汇福大晚点名：假期中，已跳过（{vac}）", "假期期间自动签到暂停。")
        return

    token = (cfg.get("user") or {}).get("token", "")
    if not token:
        username = (cfg.get("user") or {}).get("username", "")
        password = (cfg.get("user") or {}).get("password", "")
        if not (username and password):
            raise SystemExit(
                "config.yaml 未配置 token，且学号/密码不全（学号密码=智汇福大 App 登录账号）"
            )
        from src.login import login as sso_login

        token = sso_login(username, password)
        print("SSO 登录成功，已获取新 token")
        _save_token(cfg, token)

    client = AttnClient(token)
    try:
        status = query_today_task(client, cfg)
    except requests.HTTPError as e:
        # 跨午夜时段（约 0:00-1:00）服务器尚未发布当日计划时会返回 500，属正常现象。
        title = "智汇福大晚点名：服务器暂未返回今日计划"
        print(title, e)
        # 21:45 前的首跑推送提醒一次；之后的兜底跑静默，避免一晚连推多条。
        # 这个阈值只用于"推送去重"，与签到窗口/自定义 times 无关，故不接调度器。
        if now_cn().time() < datetime.time(21, 45):
            notify(cfg, title, "服务器暂时无今日计划数据（跨天时段/服务波动），本次跳过。稍后自动重试无需操作。")
        else:
            print("（兜底跑：仍无计划数据，静默跳过，不再重复推送）")
        return
    init_data = status["init"]

    if not status["need_checkin"]:
        # 静默：首跑成功时已推送过，后续兜底跑只需记日志，不再打扰
        print("已签到 / 无需操作（不推送，避免每晚重复通知）")
        return

    if not init_data.get("schoolData"):
        title = "智汇福大晚点名：今日无考勤计划"
        print(title)
        # 22:00 前的首跑推送提醒一次；之后的兜底跑静默（同样是推送去重阈值，与窗口无关）
        if now_cn().time() < datetime.time(22, 0):
            notify(cfg, title, "服务器未返回签到范围（可能今日无晚点名），未执行打卡。")
        else:
            print("（晚间兜底跑：无考勤计划，静默跳过，不重复推送）")
        return

    try:
        ok = do_checkin(client, cfg, init_data)
    except Exception as e:  # 接口报错（如不在时段/范围）要带原因推送
        title = "智汇福大晚点名：签到失败 ❌"
        print(title, e)
        notify(cfg, title, f"自动签到失败：{e}\n执行时刻 {exec_text}，请手动打开 App 签到。")
        return

    if ok:
        title = "智汇福大晚点名：签到成功 ✅"
        print(title)
        notify(cfg, title, f"今日晚点名已自动签到成功（执行时刻 {exec_text}）。")
    else:
        title = "智汇福大晚点名：不在签到范围 ❌"
        print(title)
        notify(cfg, title, f"定位校验未命中任何校区，请确认配置的经纬度。（执行时刻 {exec_text}）")


if __name__ == "__main__":
    main()
