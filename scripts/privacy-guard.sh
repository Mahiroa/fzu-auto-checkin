#!/bin/sh
# 隐私闸门：把「不能进仓库的个人信息」变成一条可执行的规则。
#
# 为什么要有它：betterleaks 只认高熵/带前缀的密钥，学号、手机号、本机绝对路径、
# 楼栋号这些**形状固定但熵很低**的值它一条都抓不到，而它们恰恰是这个仓库历史上
# 真正泄露过的东西（见 README「隐私」一节）。两道闸门分工：
#   repo-guard.sh  —— 路径形状（产物/测试/截图/配置）
#   privacy-guard  —— 内容形状（学号/手机/邮箱/本机路径/楼栋/Cookie 导出）
#   secret-scan    —— 高熵密钥（token/AK/私钥）
#
# 用法：
#   ./scripts/privacy-guard.sh            # 查暂存区（pre-commit 用，默认）
#   ./scripts/privacy-guard.sh --tree     # 查所有已跟踪文件（CI 用）
#   ./scripts/privacy-guard.sh --recent 20# 查最近 N 个提交的新增行（默认 20）
#   ./scripts/privacy-guard.sh --history  # 查全历史新增行 + 提交者身份 + 提交信息
#   ./scripts/privacy-guard.sh --message FILE  # 查一份提交信息（commit-msg 钩子用）
# 退出码：0 = 干净；1 = 命中（打印规则/位置/脱敏值）；2 = 用法或环境问题。

set -u

mode="staged"
recent_n=20
msg_file=""
while [ $# -gt 0 ]; do
  case "$1" in
    --staged)    mode="staged" ;;
    --tree)      mode="tree" ;;
    --history)   mode="history" ;;
    --recent)
      mode="recent"
      if [ $# -ge 2 ] && [ "$2" != "" ]; then
        recent_n="$2"
        shift
      fi
      case "$recent_n" in
        ''|*[!0-9]*) echo "[privacy-guard] --recent 需要一个正整数" >&2; exit 2 ;;
      esac
      ;;
    --recent=*)
      mode="recent"
      recent_n="${1#--recent=}"
      case "$recent_n" in
        ''|*[!0-9]*) echo "[privacy-guard] --recent 需要一个正整数" >&2; exit 2 ;;
      esac
      ;;
    --message)
      shift
      msg_file="${1:-}"
      if [ -z "$msg_file" ]; then
        echo "[privacy-guard] --message 需要一个文件路径" >&2
        exit 2
      fi
      mode="message"
      ;;
    *)
      echo "用法: $0 [--tree|--recent N|--history|--message FILE]" >&2
      exit 2
      ;;
  esac
  shift
done

root=$(git rev-parse --show-toplevel 2>/dev/null) || {
  echo "[privacy-guard] 不在 git 仓库里" >&2
  exit 2
}
cd "$root" || exit 2

rules="$root/scripts/privacy-guard.awk"
if [ ! -f "$rules" ]; then
  echo "[privacy-guard] 缺少规则文件：$rules" >&2
  exit 2
fi

awk_bin=$(command -v awk 2>/dev/null) || {
  echo "[privacy-guard] 找不到 awk，无法扫描（fail-closed）" >&2
  exit 2
}

rc=0

if [ "$mode" = "staged" ]; then
  # --unified=0：只要新增行，位置能直接对上新文件的行号。
  git diff --cached --no-ext-diff --no-textconv --no-color --unified=0 --diff-filter=ACMR \
    | "$awk_bin" -v mode=diff -f "$rules" || rc=1
  # 提交者身份：2026-09 那几个 commit 就是"内容干净、作者邮箱带学号"，
  # 只看 diff 永远发现不了，所以提交前把 GIT_AUTHOR/COMMITTER 也过一遍。
  {
    git var GIT_AUTHOR_IDENT 2>/dev/null
    git var GIT_COMMITTER_IDENT 2>/dev/null
  } | "$awk_bin" -v mode=ident -f "$rules" || rc=1
fi

if [ "$mode" = "message" ]; then
  # commit-msg 钩子把用户写好的消息文件交过来：注释行（# 开头）是 git 的模板残留，跳过。
  if [ ! -f "$msg_file" ]; then
    echo "[privacy-guard] 提交信息文件不存在：$msg_file" >&2
    exit 2
  fi
  "$awk_bin" -v mode=msg -v msgfile="$msg_file" -f "$rules" < "$msg_file" || rc=1
fi

if [ "$mode" = "recent" ]; then
  git log -p -n "$recent_n" --no-color --no-ext-diff --no-textconv \
    | "$awk_bin" -v mode=diff -f "$rules" || rc=1
  git log -n "$recent_n" --format='%H %ae%n%H %ce' | sort -u \
    | "$awk_bin" -v mode=meta -f "$rules" || rc=1
fi

if [ "$mode" = "tree" ]; then
  # 只看已跟踪的文本文件；-z 保证中文/空格路径不被拆开。xargs -r：没有文件时别让
  # awk 空转等 stdin（Git Bash 与 GNU 都支持这一条）。
  git grep -z -I -l -e '' -- . \
    | xargs -0 -r "$awk_bin" -v mode=plain -f "$rules" || rc=1
fi

if [ "$mode" = "history" ]; then
  # 历史里已经进去的东西：diff 能看内容，但提交者身份不在 diff 里，单独跑一遍。
  # 每条提交拆成两行（author / committer），配合 sort -u 去重，避免同一个人被报两次。
  git log -p --all --no-color --no-ext-diff --no-textconv \
    | "$awk_bin" -v mode=diff -f "$rules" || rc=1
  git log --all --format='%H %ae%n%H %ce' | sort -u \
    | "$awk_bin" -v mode=meta -f "$rules" || rc=1
  # 提交信息也进历史：`git commit -m "学号 xxx"` 这类内容同样公开。
  git log --all --format='COMMIT %h%n%B' \
    | "$awk_bin" -v mode=msg -f "$rules" || rc=1
fi

if [ "$rc" -ne 0 ]; then
  cat >&2 <<'EOF'

[privacy-guard] 上面这些命中不该进仓库。处理办法：
  1) 合成夹具：在那一行加 privacy:allow，并在注释里写清为什么是合成的；
  2) 真实值：从文件里删掉（改用环境变量/本机 config.yaml），不要靠 --no-verify 硬推；
  3) 确实是公开信息（例如项目公开的联系邮箱）：往 privacy-guard.awk 的 okdom 里加域名，
     绝不把真实值本身写进规则文件。
EOF
  exit 1
fi

case "$mode" in
  staged)  echo "[privacy-guard] 暂存区干净：没有学号/手机号/个人邮箱/本机路径/楼栋/Cookie 导出，提交者身份也干净" ;;
  tree)    echo "[privacy-guard] 已跟踪文件干净（--tree）" ;;
  recent)  echo "[privacy-guard] 最近 $recent_n 个提交干净（--recent）" ;;
  message) echo "[privacy-guard] 提交信息干净（--message）" ;;
  history) echo "[privacy-guard] 全历史未发现内容型隐私命中（提交者身份与提交信息见上）" ;;
esac
exit 0
