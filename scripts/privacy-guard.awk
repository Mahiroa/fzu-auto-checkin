# 隐私闸门规则（由 scripts/privacy-guard.sh 调用；三种输入模式共用这一份规则）
#
# 只做形状识别，不理解语义。判错了怎么办：
#   1) 合成夹具 → 在那一行加 privacy:allow（例如测试里的 13800001234 这类示例号码，
#      号码本身已经是合成形状，规则里另有 1[3-9]0000xxxx 的放行）；
#   2) 真实值 → 先想清楚它该不该进仓库；确实该进（比如公开的联系邮箱），
#      再往下面的 okdom 白名单加域名，绝不把真实值本身写进规则文件。
#
# 模式：
#   -v mode=diff   读 git diff / git log -p（暂存区、全历史）
#   -v mode=plain  读普通文件，FILENAME/FNR 当位置（--tree）
#   -v mode=meta   每行 "<commit> <email>"，只查提交者身份

BEGIN {
    if (mode == "") mode = "diff"
    n = split("example.com example.org example.net localhost users.noreply.github.com github.com", doms, " ")
    for (i = 1; i <= n; i++) okdom[doms[i]] = 1
    total = 0
}

function mask(s,   len) {
    len = length(s)
    if (len <= 5) return "***"
    return substr(s, 1, 3) "***" substr(s, len - 1)
}

function report(rule, where, value, why) {
    printf "[%-6s] %s  ->  %s  (%s)\n", rule, where, mask(value), why
    hits[rule]++
    total++
}

function email_ok(e,   d) {
    d = tolower(substr(e, index(e, "@") + 1))
    if (d in okdom) return 1
    if (d ~ /(^|\.)example\.(com|org|net)$/) return 1
    return 0
}

# 提交者身份只拦"一眼就是个人信息"的形状：学校邮箱、纯数字本地部分。
# 个人 Gmail / QQ 邮箱是作者自己选的署名，不在这里拦（换了邮箱也能正常提交）。
function ident_risk(e,   local) {
    if (tolower(e) ~ /@fzu\.edu\.cn$/) return "学校邮箱，本地部分通常是学号"
    local = tolower(substr(e, 1, index(e, "@") - 1))
    if (local ~ /^[0-9]{6,}$/) return "邮箱本地部分是纯数字（学号/手机号）"
    return ""
}

function check_ident(line, where,   e, why) {
    e = line
    if (e ~ /</) {
        sub(/^.*</, "", e)
        sub(/>.*$/, "", e)
    } else if (e ~ /@/ && e ~ / /) {
        # "<commit> <email>" 这种两段式：取带 @ 的那一段，脱敏才有意义。
        sub(/^[^ ]+ /, "", e)
    }
    why = ident_risk(e)
    if (why != "") report("IDENT", where, e, why)
}

function check_line(line, where,   rest, e, v) {
    if (line ~ /privacy:allow/) return

    # 1) 本机绝对路径：C:\Users\<name>、C:/Users/<name>、/c/Users/<name>、/home/<name>、/Users/<name>
    #    这类路径会同时泄露用户名和目录结构，截图/日志里最容易带出来。
    if (match(line, /[A-Za-z]:[\\\/]Users[\\\/][A-Za-z0-9._-]+/) ||
        match(line, /\/c\/Users\/[A-Za-z0-9._-]+/) ||
        match(line, /\/(home|Users)\/[A-Za-z0-9._-]{2,}/)) {
        report("PATH", where, substr(line, RSTART, RLENGTH), "本机绝对路径带用户名")
    }

    # 2) 邮箱：@fzu.edu.cn 必报（本地部分通常就是学号）；其它非白名单域名也报。
    rest = line
    while (match(rest, /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/)) {
        e = substr(rest, RSTART, RLENGTH)
        rest = substr(rest, RSTART + RLENGTH)
        if (tolower(e) ~ /@fzu\.edu\.cn$/)
            report("EMAIL", where, e, "校内邮箱，本地部分通常是学号")
        else if (!email_ok(e))
            report("EMAIL", where, e, "个人邮箱（不在白名单域名）")
    }

    # 3) 手机号：1[3-9] + 9 位；中间连着四个 0 的（13800001234 这类文档示例）视为合成放行。
    if (match(line, /(^|[^0-9])1[3-9][0-9]{9}([^0-9]|$)/)) {
        v = substr(line, RSTART, RLENGTH)
        gsub(/[^0-9]/, "", v)
        if (v !~ /0{4}/) report("PHONE", where, v, "手机号")
    }

    # 4) 学号：紧跟在「学号 / 账号 / student_id」后面的 8-12 位数字，或裸的 9 位 10xxxxxxxx
    #    （福大学号形状）。文档里的占位号（3219000000 这类连串 0）按合成值放行。
    if (match(line, /(学号|账号|student_?id|stu_?id)[^0-9]{0,6}[0-9]{8,12}/)) {
        v = substr(line, RSTART, RLENGTH)
        gsub(/[^0-9]/, "", v)
        if (v !~ /0{4}/) report("SID", where, v, "学号/账号")
    } else if (match(line, /(^|[^0-9A-Za-z-])10[0-9]{7}([^0-9]|$)/)) {
        v = substr(line, RSTART, RLENGTH)
        gsub(/[^0-9]/, "", v)
        if (v !~ /0{4}/) report("SID", where, v, "疑似学号")
    }

    # 5) 具体楼栋/宿舍号：签到坐标一旦具体到楼栋，就是位置隐私。
    #    命中值不打印（中文按字节截断会切坏），固定写成"具体楼栋"。
    if (line ~ /生活区[0-9]+号楼|[0-9]+号宿舍楼/) {
        report("LOC", where, "dorm-building", "具体楼栋")
    }

    # 6) 浏览器 Cookie 导出：只要"名: 长值"这种真实形态；文档里的 {"SESSION":"xxx"} 不算。
    if (match(line, /"(SESSION|JSESSIONID|Access-Token|sessionid)"[ \t]*:[ \t]*"[A-Za-z0-9._~+\/=-]{12,}"/) &&
        line !~ /SYNTHETIC/) {
        report("COOKIE", where, "cookie-export", "Cookie 导出内容")
    }
}

mode == "diff" {
    if ($0 ~ /^commit [0-9a-f]+/) c = $2
    if ($0 ~ /^\+\+\+ /) { f = $2; sub(/^b\//, "", f); next }
    if ($0 ~ /^@@/) {
        if (match($0, /\+[0-9]+/)) ln = substr($0, RSTART + 1, RLENGTH - 1) + 0
        next
    }
    if ($0 ~ /^\+/) {
        ln++
        check_line(substr($0, 2), (c != "" ? c " " : "") f ":" ln)
        next
    }
    next
}

mode == "plain" {
    check_line($0, FILENAME ":" FNR)
    next
}

mode == "meta" {
    if (NF >= 1) check_ident($0, "commit " substr($1, 1, 7))
    next
}

mode == "ident" {
    check_ident($0, "身份")
    next
}

# 提交信息：commit-msg 钩子直接喂消息文件；--history 喂 "COMMIT <sha>" 分节的消息流。
mode == "msg" {
    if ($0 ~ /^COMMIT [0-9a-f]+$/) { c = $2; next }
    if ($0 ~ /^#/) next
    check_line($0, (c != "" ? c " message:" FNR : (msgfile != "" ? msgfile ":" FNR : FILENAME ":" FNR)))
    next
}

END {
    if (total > 0) {
        printf "\n[privacy-guard] 命中 %d 处：", total
        for (r in hits) printf " %s=%d", r, hits[r]
        printf "\n"
        exit 1
    }
    exit 0
}
