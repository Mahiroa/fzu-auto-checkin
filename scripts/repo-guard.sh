#!/bin/sh
# 路径闸门：构建产物、冒烟脚本/产物、测试与测试数据、临时数据，一律不许入库。
#
# 为什么需要第二道（.gitignore 已经是第一道）：.gitignore 只拦"还没被跟踪"的文件，
# `git add -f` 一句就能绕过；而这些目录一旦进库，撤回要改历史、force push，成本远高于
# 提交前多跑 20 毫秒。CI 里也跑同一份脚本（--tree），所以绕过本地钩子的提交会被打回。
#
# 用法：
#   ./scripts/repo-guard.sh            # 查暂存区（pre-commit 用，只看本次要提交的）
#   ./scripts/repo-guard.sh --tree     # 查所有已跟踪文件（CI 用）
#   ./scripts/repo-guard.sh --all      # 两者都查
# 退出码：0 = 干净；1 = 命中（打印路径与原因）。

set -u

mode="staged"
case "${1:-}" in
  --tree) mode="tree" ;;
  --all)  mode="all" ;;
  "")     ;;
  *) echo "用法: $0 [--tree|--all]" >&2; exit 2 ;;
esac

root=$(git rev-parse --show-toplevel 2>/dev/null) || { echo "[repo-guard] 不在 git 仓库里" >&2; exit 2; }
cd "$root" || exit 2

# 每一条都是"绝不该出现在仓库里"的路径形状。检查顺序刻意从重到轻：
# 先命中哪条就报哪条，报错文案直接告诉人该怎么处理。
check() {
  # $1 = 路径；命中则打印原因并返回 0
  case "$1" in
    # ---- 构建产物 ----
    dist/*|*/dist/*)                     echo "构建产物目录（dist/）"; return 0 ;;
    build/*|*/build/*)                   echo "构建产物目录（build/）"; return 0 ;;
    *.exe|*.dll|*.so|*.dylib|*.a|*.o|*.obj|*.test|*.out)
                                         echo "编译产物 / 可执行文件"; return 0 ;;
    *.zip|*.tar|*.tgz|*.gz|*.7z|*.rar)   echo "打包产物"; return 0 ;;
    # ---- 冒烟脚本与冒烟产物 ----
    scripts/smoke.ps1|*/scripts/smoke.ps1)
                                         echo "本机冒烟脚本（含本机测试密码）"; return 0 ;;
    *smoke*.exe|*smoke*.log|*smoke*.json|*fzucheckin-smoke*)
                                         echo "冒烟产物"; return 0 ;;
    scripts/ui-verify.ps1|scripts/ui-verify.mjs|*/scripts/ui-verify.*)
                                         echo "本机浏览器验收编排"; return 0 ;;
    # ---- 测试与测试数据 ----
    tests/*|*/tests/*)                   echo "测试目录（tests/）"; return 0 ;;
    *_test.go|*_test.py)                 echo "单元测试文件"; return 0 ;;
    testdata/*|*/testdata/*)             echo "测试数据 / 向量"; return 0 ;;
    *.pytest_cache/*|*/.pytest_cache)    echo "pytest 缓存"; return 0 ;;
    scripts/gen_parity.py)               echo "一致性语料生成器"; return 0 ;;
    # ---- 临时数据 / 运行时数据 ----
    data/*|*/data/*|data-*/*|*/data-*/*) echo "运行时数据目录（含凭据哈希与运行历史）"; return 0 ;;
    *.jsonl|*.log|*.bak|*.tmp|*.swp|*.swo)
                                         echo "临时数据 / 日志 / 备份"; return 0 ;;
    *.key|secret.key|*.pem|*.p12|*.pfx)  echo "密钥文件"; return 0 ;;
    *.db|*.sqlite|*.sqlite3)             echo "本地数据库"; return 0 ;;
    *.prof|*.pprof|*.dmp|*.trace)        echo "性能/崩溃转储"; return 0 ;;
    # ---- 本机现场与配置 ----
    .recon/*|*/.recon/*)                 echo "本机抓包现场与工具目录"; return 0 ;;
    .shots/*|*/.shots/*)                 echo "本机截图 / 探针暂存"; return 0 ;;
    deploy/*|*/deploy/*)                 echo "本机部署记录（真实路径、域名、备份名）"; return 0 ;;
    docs/verify-shots/*)                 echo "验收截图"; return 0 ;;
    config.yaml|config.yml|*/config.yaml|*/config.yml)
                                         echo "真实配置（含凭据与坐标）"; return 0 ;;
    .env|*/.env|.env.*|*/.env.*)         echo "环境变量文件"; return 0 ;;
    # ---- 编辑器 / 系统垃圾 ----
    .DS_Store|*/.DS_Store)               echo "macOS 目录垃圾"; return 0 ;;
    node_modules/*|*/node_modules/*)     echo "依赖目录"; return 0 ;;
    .idea/*|*/.idea/*|.vs/*|*/.vs/*)     echo "IDE 工程目录"; return 0 ;;
  esac
  return 1
}

report() {
  echo ""
  echo "[repo-guard] 这些路径不能入库（$1）："
  echo "$2" | sed 's/^/    /'
  echo ""
  echo "  处理办法：种子文件进 .gitignore 之外，把产物生成到别处或直接删掉；"
  echo "  确实需要版本控制的话，先改这条规则并在 PR 里说明理由。"
  echo "  想跳过检查：git commit --no-verify —— 但 CI 的 repo-guard 仍会拦下这次 push。"
}

bad=""
if [ "$mode" = "staged" ] || [ "$mode" = "all" ]; then
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    if reason=$(check "$path"); then
      bad="$bad$path  <= $reason
"
    fi
  done <<EOF
$(git diff --cached --name-only --diff-filter=ACMR 2>/dev/null)
EOF
fi

if [ "$mode" = "tree" ] || [ "$mode" = "all" ]; then
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    if reason=$(check "$path"); then
      bad="$bad$path  <= $reason
"
    fi
  done <<EOF
$(git ls-files 2>/dev/null)
EOF
fi

if [ -n "$bad" ]; then
  report "$mode" "$bad"
  exit 1
fi

echo "[repo-guard] 干净：没有产物 / 测试 / 临时数据（$mode）"
exit 0
