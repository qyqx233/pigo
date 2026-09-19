#!/usr/bin/env bash
# 构建给 pigo-server 沙箱用的专用 Python 环境：一整套独立、可整体拷贝的
# CPython（uv 管理的 python-build-standalone 版本），预装 requirements.txt
# 里的包。
#
# 为什么不用 venv：venv 的 bin/python 是指向 uv 缓存的软链接，离开本机或
# 换个路径就失效。这里直接复制完整的 CPython 安装，再把包装进去：
#   - 目录里的软链接都是内部的相对链接；
#   - 运行时只依赖系统 glibc；
#   - bin/ 下各命令的开头改写为"按脚本所在位置找解释器"，
#     所以整个目录可以移动、拷到别的机器、只读挂到沙箱里任意路径。
#
# 用法：
#   cmd/pigo-server/sandbox-python/build.sh [-p 3.12] [-o 目标目录] [-r 包清单] [-f]
#
#   -p  Python 版本，默认 3.12
#   -o  目标目录，默认 $PIGO_SANDBOX_PYTHON 或 ~/.local/share/pigo/sandbox-python
#   -r  包清单，默认同目录的 requirements.txt
#   -f  目标已存在时替换它：先完整构建好再换上，失败不影响旧环境；旧环境
#       留作 <目标>.prev（正在运行的沙箱仍在用），下次重建时删除
#
# 以后加包：在 requirements.txt 里加上，带 -f 重新构建即可。
set -eu

info() { printf '%s\n' "build-sandbox-python: $*" >&2; }
err()  { printf '%s\n' "build-sandbox-python: error: $*" >&2; exit 1; }

here=$(cd "$(dirname "$0")" && pwd)
PYVER=3.12
OUT=${PIGO_SANDBOX_PYTHON:-$HOME/.local/share/pigo/sandbox-python}
REQS=$here/requirements.txt
FORCE=0
while getopts "p:o:r:fh" opt; do
	case "$opt" in
		p) PYVER=$OPTARG ;;
		o) OUT=$OPTARG ;;
		r) REQS=$OPTARG ;;
		f) FORCE=1 ;;
		h) sed -n '2,22p' "$0"; exit 0 ;;
		*) err "未知参数，见 -h" ;;
	esac
done

command -v uv >/dev/null 2>&1 || err "需要 uv（https://docs.astral.sh/uv/）"
[ -f "$REQS" ] || err "找不到包清单 $REQS"
case "$OUT" in /*) ;; *) OUT=$(pwd)/$OUT ;; esac
OUT=${OUT%/}
if [ -e "$OUT" ] && [ "$FORCE" != 1 ]; then
	err "$OUT 已存在；要替换请加 -f"
fi
parent=$(dirname "$OUT")
mkdir -p "$parent" 2>/dev/null || true
[ -w "$parent" ] || err "$parent 不可写。目标在 /opt 等系统目录时，先 sudo mkdir -p $OUT && sudo chown \$USER $OUT 的上级目录，或换一个目标目录"

# 1. 找到（必要时下载）uv 管理的独立 CPython，定位它的安装目录。
info "准备 CPython $PYVER"
uv python install "$PYVER" >/dev/null 2>&1 || err "uv python install $PYVER 失败"
py=$(uv python find --managed-python --no-project "$PYVER") || err "找不到 uv 管理的 CPython $PYVER"
src=$(dirname "$(dirname "$(readlink -f "$py")")")
minor=$(basename "$(readlink -f "$py")")          # 形如 python3.12
[ -d "$src/lib/$minor" ] || err "$src 不像一套完整的 CPython 安装"

# 2. 复制到目标旁边的临时目录里构建；成功后再换上，失败不留半成品。
# 用真实路径（pwd -P）：目标在软链接目录下时（比如 home 本身是个软链接），
# 后面按前缀判断"链接是否指向目录内"的检查才不会误报。
build=$(mktemp -d "$parent/.sandbox-python-build.XXXXXX")
build=$(cd "$build" && pwd -P)
cleanup() { rm -rf "$build"; }
trap cleanup EXIT
info "复制 $src"
cp -a "$src/." "$build/"

# 3. 去掉 uv 的"外部管理"标记：这份复制品是专用环境，不再归 uv 管理。
rm -f "$build/lib/$minor/EXTERNALLY-MANAGED"

# 4. 装包。--link-mode=copy：文件真正复制进来，不硬链接到 uv 缓存。
info "安装 $REQS 中的包"
uv pip install --python "$build/bin/$minor" --system --link-mode=copy -r "$REQS" \
	|| err "装包失败"

# 5. 改写 bin/ 下命令脚本的开头：uv 装包时写进去的是解释器的绝对路径，目录
#    一移动就失效。uv 有两种写法：路径短时是一行 #!/路径/python；路径超过内核
#    对这一行的长度限制（127 字节）时，改用 /bin/sh 包一层。两种都换成按脚本
#    自身所在位置找解释器（uv 可重定位环境用的同一个写法）。
"$build/bin/$minor" - "$build" "$minor" <<'EOF'
import os, sys
root, minor = sys.argv[1], sys.argv[2]
interp = os.path.join(root, "bin", "python").encode()
direct = b"#!" + interp                           # 一行的写法
wrapped = b"#!/bin/sh\n'''exec' '" + interp       # /bin/sh 包一层的写法（占三行）
stub = ("#!/bin/sh\n'''exec' \"$(dirname -- \"$(realpath -- \"$0\")\")\"/%s \"$0\" \"$@\"\n' '''\n" % minor).encode()
changed = 0
for name in sorted(os.listdir(os.path.join(root, "bin"))):
    path = os.path.join(root, "bin", name)
    if os.path.islink(path) or not os.path.isfile(path):
        continue
    with open(path, "rb") as f:
        data = f.read()
    if data.startswith(wrapped):
        head = 3
    elif data.startswith(direct):
        head = 1
    else:
        continue
    parts = data.split(b"\n", head)
    rest = parts[head] if len(parts) > head else b""
    with open(path, "wb") as f:
        f.write(stub + rest)
    changed += 1
print("build-sandbox-python: 改写了 %d 个命令脚本" % changed, file=sys.stderr)
EOF

# 6. 检查可移植性：不能有指向目录外的软链接，也不能有文本文件写死构建路径。
outside=$(find "$build" -type l | while read -r l; do
	t=$(readlink -f "$l" || true)
	case "$t" in "$build"/*) ;; *) printf '%s\n' "$l" ;; esac
done)
[ -z "$outside" ] || err "有软链接指向目录外，无法整体拷贝：$(printf '%s' "$outside" | head -3)"
leaked=$(grep -rIl --exclude-dir=__pycache__ "$build" "$build" 2>/dev/null | head -5 || true)
[ -z "$leaked" ] || err "以下文件仍写着构建路径，移动后会失效：$leaked"

# 7. 记录这次构建装了什么，方便复现。
{
	printf '# 构建于 %s，CPython %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$("$build/bin/$minor" -c 'import platform; print(platform.python_version())')"
	uv pip freeze --python "$build/bin/$minor" --system
} >"$build/PIGO-PACKAGES.txt"

# 8. 换上。旧环境不立刻删除，而是留作 $OUT.prev：正在运行的沙箱挂载的是旧
#    目录，删掉它会让那些会话里的 Python 突然消失。新启动的沙箱用新环境；
#    空闲的沙箱会被回收（-idle，默认 30 分钟）。下次重建时再删上一份。
if [ -e "$OUT" ]; then
	rm -rf "$OUT.prev"
	mv "$OUT" "$OUT.prev"
	mv "$build" "$OUT"
	info "旧环境留在 $OUT.prev，正在运行的沙箱仍在使用它；确认都已重启后可以删除"
else
	mv "$build" "$OUT"
fi
trap - EXIT

# 9. 在最终位置做一次冒烟测试：解释器、几个关键包、改写过的命令。
"$OUT/bin/python3" -c 'import pandas, openpyxl, pdfplumber, pypdf, pymupdf, docx, pptx, bs4' \
	|| err "冒烟测试失败：关键包无法导入"
[ ! -x "$OUT/bin/pip" ] || "$OUT/bin/pip" --version >/dev/null || err "冒烟测试失败：bin/pip 无法运行"

info "完成：$OUT（$(du -sh "$OUT" | cut -f1)），包清单见 $OUT/PIGO-PACKAGES.txt"
info "挂进沙箱后：解释器在 <挂载点>/bin/python3；以 -sandbox-tool python=$OUT 启动 pigo-server（见 spec/sandbox-toolchains.md）"
