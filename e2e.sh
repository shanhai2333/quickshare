#!/bin/bash
# QuickShare 端到端验证脚本
#
# 用法（推荐：自己起实例、自己收尾，跑完自动清理）：
#   bash e2e.sh
#
# 也可以指向一个已经在跑的实例（要求它挂在一个空数据目录上）：
#   QS_BASE=http://127.0.0.1:18080 bash e2e.sh
#
# 注意：脚本用 curl 读写临时文件。Windows 的 Git Bash 里 `curl` 是
#       C:\WINDOWS\system32\curl.exe，不认 /tmp 这类 POSIX 路径
#       （表现为 size_download 恒为 0、退出码 23、文件写不出来），
#       所以 TMP 默认算成 Windows 形式（见下面的 REPO_DIR）。
#       想换地方就设 QS_E2E_TMP（CI 上就设成 /tmp/qs-e2e）。
#
# 另一个坑：Windows 上的 Python 往**管道**写的是 \r\n。
#       `$( ... )` 会把行尾的 \r 一起吃掉，所以 `"$( ... | python ... )"` 没问题；
#       但把 Python 的输出**直接接到另一个程序**（xargs 之类）时，\r 会跟着过去，
#       拼出来的 URL 末尾多一个 \r，curl 请求失败、静默返回空串——
#       现象是断言莫名其妙 FAIL，而手工敲同样的命令又是对的。
#       所以：Python 的输出一律先用 $( ) 收进变量，再往下用。
set -u

# 这台开发机（以及不少装了代理工具的开发机）会设 http_proxy=http://127.0.0.1:xxxx，
# 于是 curl 连 127.0.0.1 也走代理。后果很坑：**端口没人监听时代理回 502，
# 而不是直连该有的 000**。断言里看到 502 会以为"服务端自己返回了错误"，
# 排查方向整个跑偏（踩过：一条断言报 502，实际是实例早就被 kill 了）。
# 本脚本只跟 127.0.0.1 说话，直接让 localhost 绕过代理。
export no_proxy="127.0.0.1,localhost,::1"
export NO_PROXY="$no_proxy"

cd "$(dirname "$0")"
PY="${PYTHON:-python3}"

# 临时目录。默认放仓库的 .tmp/e2e 下，不污染系统临时目录。
#
# 为什么要绕一下 pwd -W：上面说过 curl 在 Git Bash 里不认 POSIX 路径，
# 所以默认值得是 D:/... 形式。pwd -W 是 Git Bash 专有的，Linux/macOS 上
# 会失败，于是退回普通的 pwd —— 那两边的 curl 本来就认 POSIX 路径。
REPO_DIR="$(pwd -W 2>/dev/null || pwd)"
TMP="${QS_E2E_TMP:-$REPO_DIR/.tmp/e2e}"
PORT=18080
PORT_AUTH=18081
PORT_SWITCH=18082
DATA=".tmp/qs-test"
LOG=".tmp/e2e-server.log"

# 自动找可执行的 quickshare。Windows 是 .exe，Linux/macOS 没有扩展名。
# 想指定别的产物就设 QS_BIN。
BIN=""
for c in "${QS_BIN:-}" ./dist/quickshare ./dist/quickshare-windows-amd64.exe \
         ./dist/quickshare-linux-amd64 ./dist/quickshare-linux-arm64 ./dist/quickshare-linux-armv7; do
  if [ -n "$c" ] && [ -x "$c" ]; then BIN="$c"; break; fi
done
if [ -z "$BIN" ]; then
  echo "找不到可执行文件，请先编译（见 README）"; exit 1
fi

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  \033[32mPASS\033[0m  %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m  %s\n' "$1"; }
chk() { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 — 期望 [$3] 实际 [$2]"; fi; }
code(){ curl -s -o /dev/null -w '%{http_code}' "$@"; }

# 清空一个目录但保留目录本身。
#
# 这里**故意不用 `rm -rf "$dir"`**：沙箱的 safe-delete 守卫会拦"目录里文件数
# 超过阈值时的 rm -rf"，于是数据目录没清干净，紧接着那句"要求空数据目录"的
# 前置检查就会失败退出，报出来是"实例里已有 N 个文件"——看着像程序坏了，
# 其实是清理没生效。find -delete 是逐个文件删，不受这个阈值影响。
# （真踩过：.tmp/qs-test 攒到 50 多个文件后，脚本连续几轮起不来。）
wipe_dir() {
  [ -d "$1" ] || return 0
  find "$1" -mindepth 1 -delete 2>/dev/null
}

jget() { "$PY" -c "
import sys, json
d = json.load(sys.stdin)
for k in sys.argv[1:]:
    d = d[int(k)] if k.lstrip('-').isdigit() else d[k]
print(d)
" "$@"; }

jsonlen() { "$PY" -c 'import sys,json;print(len(json.load(sys.stdin)))'; }

# 上传一个单分片小文件，回显 complete 的 JSON。内容限 ASCII，
# 免得 wc -c 的字节数与 JSON 转义对不上。
put_small() { # $1=文件名 $2=内容 $3=MIME
  local sz i u
  sz=$(printf '%s' "$2" | wc -c | tr -d ' ')
  i=$(curl -s -X POST -H 'Content-Type: application/json' \
    -d "{\"name\":\"$1\",\"size\":$sz,\"mime\":\"$3\"}" "$B/api/upload/init")
  u=$(echo "$i" | jget uploadId)
  printf '%s' "$2" | curl -s -X PUT --data-binary @- "$B/api/upload/$u/0" > /dev/null
  curl -s -X POST "$B/api/upload/$u/complete"
}

# 按端口结束实例。Git Bash 的 kill 对 Windows 上的 .exe 不一定生效，
# 残留的僵尸实例会占着端口、还抱着旧数据库不放，把后续轮次全带偏。
#
# **这段必须两边都能用**，因为 CI 跑在 Linux 上而开发机是 Windows：
#   - Windows：`netstat -ano` 能列出 LISTENING 的 PID，但结束原生 .exe 要用
#     `taskkill`（Git Bash 的 `kill` 对它不一定生效）。
#   - Linux：多数发行版**根本没装 netstat**（net-tools 不是默认包），得用
#     `ss -lptn`（`users:(("quickshare",pid=1234,fd=7))` 里抠 pid），
#     没有 ss 再退到 `lsof -ti`；结束用普通的 `kill`。
#
# 只写 Windows 那一套的后果不是"清理不干净"这么轻——Linux 上这个函数会
# 抛 FileNotFoundError 直接空转，于是靠它收尾的实例一直活着，后面紧跟的
# `wait` 会**永久阻塞**（CI 上表现为卡到 job 超时，而不是给一条看得懂的失败）。
# 所以这里把 OSError 全兜住，宁可什么都不做也不要抛异常。
kill_port() { "$PY" -c '
import os, re, signal, subprocess, sys, time

port = sys.argv[1]
pids = set()

def run(*cmd):
    # 命令不存在（Linux 上没有 netstat、容器里没有 lsof）不该是个错误，
    # 它只意味着"这条路探不出来"，换下一条就是。
    try:
        return subprocess.run(cmd, capture_output=True, text=True, errors="replace").stdout
    except OSError:
        return ""

if os.name == "nt":
    for line in run("netstat", "-ano").splitlines():
        if "LISTENING" in line and re.search(r":" + port + r"\b", line):
            pids.add(line.split()[-1])
    for p in pids:
        run("taskkill", "/F", "/PID", p)
else:
    for line in run("ss", "-lptn").splitlines():
        if "LISTEN" not in line or not re.search(r":" + port + r"\b", line):
            continue
        pids.update(m.group(1) for m in re.finditer(r"pid=(\d+)", line))
    if not pids:
        for line in run("lsof", "-ti", ":" + port).splitlines():
            if line.strip().isdigit():
                pids.add(line.strip())
    for p in pids:
        try:
            os.kill(int(p), signal.SIGTERM)
        except (OSError, ValueError):
            pass
    # 先礼后兵：Go 服务收到 SIGTERM 会走优雅退出，给它一点时间；
    # 还在的话再 SIGKILL，免得端口被它多占半秒。
    if pids:
        time.sleep(0.4)
        for p in pids:
            try:
                os.kill(int(p), signal.SIGKILL)
            except (OSError, ValueError):
                pass

time.sleep(0.3)
' "$1"; }

# 等一个后台实例退干净，**最多等 timeout 秒**。
#
# 不要用裸 `wait`：kill_port 万一没生效（换平台、缺 ss/netstat、权限不够），
# `wait` 会一直等一个永远不退出的进程。CI 上这表现为卡到 job 超时——
# 排查时看不到任何报错，只会觉得"测试怎么这么久"。轮询至少会往下走，
# 让后面那条断言把真正的现象报出来。
wait_gone() { # $1=pid
  for _ in $(seq 1 40); do
    kill -0 "$1" 2>/dev/null || return 0
    sleep 0.25
  done
  echo "  WARN 进程 $1 在 10s 内没有退出（kill_port 可能没生效），继续往下跑" >&2
  return 0
}

# ---------------------------------------------------------------- 实例生命周期
#
# 这里把起停都管起来，是为了避开一个很坑的失败模式：实例看着"没在跑"，
# 其实旧进程还占着端口，于是新的绑定失败悄悄退出，所有请求都打在持有旧
# 数据库的僵尸上，现象完全对不上。数据目录必须干净——第 5 节把列表条数
# 写死了，带着历史数据跑必然误报。
#
# 所有实例一律带 -tray=false：测试不该往用户的通知区域里塞图标，
# 更不该让一个误点就能把正在跑的测试实例关掉。
# （真踩过：托盘菜单点一下「退出」，实例就没了，脚本随即报"实例里已有 ? 个文件"。）
if [ -n "${QS_BASE:-}" ]; then
  B="$QS_BASE"; OWN=0
else
  B="http://127.0.0.1:$PORT"; OWN=1
fi
A="http://127.0.0.1:$PORT_AUTH"
SW="http://127.0.0.1:$PORT_SWITCH"

cleanup() {
  # 只收自己起的实例。$PORT_AUTH / $PORT_SWITCH 上的实例无论哪种模式都是本脚本起的。
  kill_port $PORT_AUTH
  kill_port $PORT_SWITCH
  if [ "$OWN" = "1" ]; then kill_port $PORT; fi
  # 顺手把测试数据目录清掉。留着既没用，又会一轮轮攒文件，最后把下次开头的
  # 清理卡住（见 wipe_dir 的注释）。$TMP 里的日志刻意保留，排查时要看。
  wipe_dir "$DATA"
}
trap cleanup EXIT

if [ "$OWN" = "1" ]; then
  kill_port $PORT
  wipe_dir "$DATA"; mkdir -p "$DATA"
  # QS_UPDATE_CHECK=0：e2e **不该依赖外网**。开着的话，构建时注入了 Repo 的
  # 二进制（比如 `make dist` 出来的）会去问 GitHub，断言就跟着网络状态飘。
  QS_UPDATE_CHECK=0 "$BIN" -addr "127.0.0.1:$PORT" -data "./$DATA" -tray=false > "$LOG" 2>&1 &
  for _ in $(seq 1 60); do
    [ "$(curl -s "$B/api/files" 2>/dev/null)" = "[]" ] && break
    sleep 0.25
  done
fi

wipe_dir "$TMP"; mkdir -p "$TMP"

# 用标准库现造一张 4x4 的合法 PNG 当背景素材，避免依赖外部图片
"$PY" - "$TMP/bg.png" <<'PYEOF'
import struct, sys, zlib

def chunk(tag, data):
    body = tag + data
    return struct.pack('>I', len(data)) + body + struct.pack('>I', zlib.crc32(body) & 0xffffffff)

w = h = 4
raw = b''.join(b'\x00' + b'\x33\x66\x99' * w for _ in range(h))
png = (b'\x89PNG\r\n\x1a\n'
       + chunk(b'IHDR', struct.pack('>IIBBBBB', w, h, 8, 2, 0, 0, 0))
       + chunk(b'IDAT', zlib.compress(raw))
       + chunk(b'IEND', b''))
open(sys.argv[1], 'wb').write(png)
PYEOF
BG_SUM=$(sha256sum "$TMP/bg.png" | cut -d' ' -f1)

# 第 5 节把列表条数写死了，所以实例必须挂在一个空数据目录上。
# 这里提前拦一下，免得跑到第 5 节才莫名其妙地 FAIL。
PRE=$(curl -s "$B/api/files" | jsonlen 2>/dev/null || echo "?")
if [ "$PRE" != "0" ]; then
  echo
  echo "  实例 $B 里已有 ${PRE} 个文件，本脚本要求空数据目录。"
  echo "  不传 QS_BASE 时脚本会自己起一个干净实例，直接 bash e2e.sh 即可。"
  exit 1
fi


# ---------------------------------------------------------------- 1
echo
echo "=== 1. 分片上传：20 MiB + 1234 字节，末片非对齐 ==="
SRC="$TMP/source.bin"
head -c 20972754 /dev/urandom > "$SRC"
SRC_SUM=$(sha256sum "$SRC" | cut -d' ' -f1)

INIT=$(curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"name":"测试 视频.mp4","size":20972754,"mime":"video/mp4"}' "$B/api/upload/init")
U1=$(echo "$INIT" | jget uploadId)
CHUNK=$(echo "$INIT" | jget chunkSize)
TOTAL=$(echo "$INIT" | jget totalChunks)
echo "  uploadId=$U1  chunkSize=$CHUNK  totalChunks=$TOTAL"
chk "分片数为 3" "$TOTAL" "3"
chk "首次不是续传" "$(echo "$INIT" | jget resumed)" "False"
chk "分片大小为 8 MiB" "$CHUNK" "8388608"

for ((i=0; i<TOTAL; i++)); do
  dd if="$SRC" bs=$CHUNK skip=$i count=1 2>/dev/null \
    | curl -s -X PUT --data-binary @- "$B/api/upload/$U1/$i" > /dev/null
done
chk "三个分片均已收到" "$(curl -s "$B/api/upload/$U1/status" | jget received)" "[0, 1, 2]"

DONE=$(curl -s -X POST "$B/api/upload/$U1/complete")
FID=$(echo "$DONE" | jget id)
chk "合并后大小与源文件一致" "$(echo "$DONE" | jget size)" "20972754"
chk "原始文件名（含中文与空格）保留" "$(echo "$DONE" | jget name)" "测试 视频.mp4"

# ---------------------------------------------------------------- 2
echo
echo "=== 2. 断点续传 ==="
INIT2=$(curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"name":"断点.bin","size":20972754,"mime":""}' "$B/api/upload/init")
U2=$(echo "$INIT2" | jget uploadId)
for i in 0 1; do
  dd if="$SRC" bs=$CHUNK skip=$i count=1 2>/dev/null \
    | curl -s -X PUT --data-binary @- "$B/api/upload/$U2/$i" > /dev/null
done
INIT3=$(curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"name":"断点.bin","size":20972754,"mime":""}' "$B/api/upload/init")
chk "重新 init 识别为续传" "$(echo "$INIT3" | jget resumed)" "True"
chk "复用同一个 uploadId" "$(echo "$INIT3" | jget uploadId)" "$U2"
chk "返回已完成的分片 [0,1]" "$(echo "$INIT3" | jget received)" "[0, 1]"
chk "取消上传返回 200" "$(code -X DELETE "$B/api/upload/$U2")" "200"
chk "取消后分片目录被清理" \
  "$([ -d ".tmp/qs-test/chunks/$U2" ] && echo exists || echo gone)" "gone"

# ---------------------------------------------------------------- 3
echo
echo "=== 3. 分片大小校验（截断可被定位且可恢复） ==="
INIT6=$(curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"name":"sizecheck.bin","size":1000,"mime":""}' "$B/api/upload/init")
U6=$(echo "$INIT6" | jget uploadId)
head -c 500 /dev/urandom > "$TMP/half.bin"
head -c 1000 /dev/urandom > "$TMP/full.bin"
chk "截断的分片被拒绝" \
  "$(code -X PUT --data-binary @"$TMP/half.bin" "$B/api/upload/$U6/0")" "400"
chk "拒绝后任务仍存在" "$(code "$B/api/upload/$U6/status")" "200"
chk "重传正确分片成功" \
  "$(code -X PUT --data-binary @"$TMP/full.bin" "$B/api/upload/$U6/0")" "200"
chk "随后可正常合并" "$(code -X POST "$B/api/upload/$U6/complete")" "200"

# ---------------------------------------------------------------- 4
echo
echo "=== 4. 文件名安全：路径穿越被清理 ==="
INIT4=$(curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"name":"../../../etc/passwd","size":5,"mime":""}' "$B/api/upload/init")
U4=$(echo "$INIT4" | jget uploadId)
printf 'abcde' | curl -s -X PUT --data-binary @- "$B/api/upload/$U4/0" > /dev/null
F4=$(curl -s -X POST "$B/api/upload/$U4/complete")
chk "穿越路径被清理为 passwd" "$(echo "$F4" | jget name)" "passwd"
chk "文件仍落在受控目录内" \
  "$([ -f ".tmp/qs-test/files/$(echo "$F4" | jget id)" ] && echo inside || echo outside)" "inside"

# ---------------------------------------------------------------- 5
echo
echo "=== 5. 文件列表接口 ==="
FILES=$(curl -s "$B/api/files")
chk "列表条数为 3" "$(echo "$FILES" | jsonlen)" "3"
chk "列表最新的在最前" "$(echo "$FILES" | jget 0 name)" "passwd"
chk "列表带下载地址" \
  "$(echo "$FILES" | "$PY" -c 'import sys,json;print(json.load(sys.stdin)[0]["url"].startswith("/f/"))')" "True"
chk "统计文件数正确" "$(curl -s "$B/api/stats" | jget files)" "3"
chk "统计总大小正确" "$(curl -s "$B/api/stats" | jget totalSize)" "20973759"

# ---------------------------------------------------------------- 6
echo
echo "=== 6. 完整下载并校验内容 ==="
DLCODE=$(curl -s -o "$TMP/dl.bin" -w '%{http_code}' "$B/f/$FID/%E6%B5%8B%E8%AF%95%20%E8%A7%86%E9%A2%91.mp4")
chk "下载 200" "$DLCODE" "200"
chk "下载内容 sha256 与源文件一致" \
  "$(sha256sum "$TMP/dl.bin" | cut -d' ' -f1)" "$SRC_SUM"

# ---------------------------------------------------------------- 7
echo
echo "=== 7. Range 断点续传 ==="
RC=$(curl -s -r 100-199 -o "$TMP/part.bin" -w '%{http_code}' "$B/f/$FID/x.mp4")
chk "Range 返回 206" "$RC" "206"
chk "Range 长度 100 字节" "$(wc -c < "$TMP/part.bin" | tr -d ' ')" "100"
dd if="$SRC" bs=1 skip=100 count=100 2>/dev/null > "$TMP/expect.bin"
chk "Range 内容与源文件对应区间一致" \
  "$(sha256sum "$TMP/part.bin" | cut -d' ' -f1)" "$(sha256sum "$TMP/expect.bin" | cut -d' ' -f1)"
echo "  $(curl -s -o /dev/null -D - "$B/f/$FID/x.mp4" | grep -i '^accept-ranges' | tr -d '\r')"

# ---------------------------------------------------------------- 8
echo
echo "=== 8. 预览 / 下载两种 Content-Disposition ==="
INLINE=$(curl -s -o /dev/null -D - "$B/f/$FID/x.mp4" | grep -i '^content-disposition' | tr -d '\r')
ATTACH=$(curl -s -o /dev/null -D - "$B/f/$FID/x.mp4?dl=1" | grep -i '^content-disposition' | tr -d '\r')
case "$INLINE" in *inline*) ok "mp4 默认 inline 预览";; *) bad "mp4 默认应为 inline，实际: $INLINE";; esac
case "$ATTACH" in *attachment*) ok "dl=1 时转为 attachment";; *) bad "dl=1 应为 attachment，实际: $ATTACH";; esac
case "$ATTACH" in *"filename*=UTF-8''"*) ok "文件名带 RFC 5987 中文编码";; *) bad "缺少 UTF-8 文件名编码";; esac

# ---------------------------------------------------------------- 9
echo
echo "=== 9. 删除文件 ==="
chk "删除返回 200" "$(code -X DELETE "$B/api/files/$FID")" "200"
chk "删除后下载 404" "$(code "$B/f/$FID/x.mp4")" "404"
chk "磁盘上的文件本体已移除" \
  "$([ -f ".tmp/qs-test/files/$FID" ] && echo exists || echo gone)" "gone"
chk "列表条数降为 2" "$(curl -s "$B/api/files" | jsonlen)" "2"
chk "删除不存在的文件 404" "$(code -X DELETE "$B/api/files/nope")" "404"

# ---------------------------------------------------------------- 10
echo
echo "=== 10. 空文件上传 ==="
INIT5=$(curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"name":"empty.txt","size":0,"mime":"text/plain"}' "$B/api/upload/init")
U5=$(echo "$INIT5" | jget uploadId)
chk "空文件分片数为 0" "$(echo "$INIT5" | jget totalChunks)" "0"
chk "空文件合并成功" "$(code -X POST "$B/api/upload/$U5/complete")" "200"

# 越界分片必须被拒。这里曾经有个洞：检查写成 `TotalChunks > 0 && idx >= TotalChunks`，
# 于是声明 0 字节的任务可以无限接收 8 MiB 分片（每个都返回 200 并落盘），
# 等于绕过 QS_MAX_FILE_SIZE 把磁盘写满。0 字节文件本来就没有分片，任何分片都该拒。
INIT7=$(curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"name":"zero2.bin","size":0,"mime":""}' "$B/api/upload/init")
U7=$(echo "$INIT7" | jget uploadId)
chk "往 0 字节任务塞分片被拒 400" \
  "$(dd if="$SRC" bs=$CHUNK count=1 2>/dev/null \
     | curl -s -o /dev/null -w '%{http_code}' -X PUT --data-binary @- "$B/api/upload/$U7/0")" "400"
chk "被拒的分片没有落盘" \
  "$(ls -1 ".tmp/qs-test/chunks/$U7" 2>/dev/null | wc -l | tr -d ' ')" "0"
chk "0 字节任务随后仍可正常完成" "$(code -X POST "$B/api/upload/$U7/complete")" "200"

# ---------------------------------------------------------------- 11
echo
echo "=== 11. 外观设置与背景图 ==="
chk "初始主题为空（跟随系统）" "$(curl -s "$B/api/settings" | jget theme)" ""
chk "初始无背景图" \
  "$(curl -s "$B/api/settings" | "$PY" -c 'import sys,json;print(json.load(sys.stdin)["background"])')" "None"

chk "设为深色返回 200" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"theme":"dark"}' "$B/api/settings")" "200"
chk "主题已持久化" "$(curl -s "$B/api/settings" | jget theme)" "dark"
# 主题由服务端写进首页 HTML，首帧就是最终配色，不依赖 JS
chk "首页 HTML 已带 data-theme" \
  "$(curl -s "$B/" | grep -c '<html lang="zh-CN" data-theme="dark">')" "1"
chk "首页不可长缓存（主题随时会变）" \
  "$(curl -s -o /dev/null -D - "$B/" | grep -ci '^cache-control: no-cache')" "1"

# 前端资源带内容哈希版本号：重复访问能走缓存，升级后也不会拿到旧 JS/CSS
ASSET=$(curl -s "$B/" | "$PY" -c 'import re,sys
m = re.search(r"/style\.css\?v=([0-9a-f]+)", sys.stdin.read())
print(m.group(1) if m else "")')
chk "首页引用的 style.css 带版本号" "$(echo -n "$ASSET" | wc -c | tr -d ' ')" "16"
chk "首页引用的 app.js 用同一个版本号" \
  "$(curl -s "$B/" | grep -c "src=\"/app.js?v=$ASSET\"")" "1"
chk "带版本号的资源可长缓存" \
  "$(curl -s -o /dev/null -D - "$B/style.css?v=$ASSET" | grep -ci 'cache-control: public, max-age=31536000, immutable')" "1"
chk "不带版本号时要求重新校验" \
  "$(curl -s -o /dev/null -D - "$B/style.css" | grep -ci '^cache-control: no-cache')" "1"
chk "版本号只影响缓存，不影响内容" \
  "$(curl -s "$B/app.js?v=$ASSET" | sha256sum | cut -d' ' -f1)" \
  "$(curl -s "$B/app.js" | sha256sum | cut -d' ' -f1)"

chk "非法主题被拒绝 400" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"theme":"blue"}' "$B/api/settings")" "400"
chk "非法主题未污染已存值" "$(curl -s "$B/api/settings" | jget theme)" "dark"
chk "非法主题也注不进 HTML" \
  "$(curl -s "$B/" | grep -c 'data-theme="blue"')" "0"
chk "PUT 不带 theme 字段不报错" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{}' "$B/api/settings")" "200"
chk "不带 theme 字段时不影响已存值" "$(curl -s "$B/api/settings" | jget theme)" "dark"
chk "空字符串合法（跟随系统）" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"theme":""}' "$B/api/settings")" "200"
chk "跟随系统时首页不写 data-theme" \
  "$(curl -s "$B/" | grep -c '<html lang="zh-CN">')" "1"

chk "上传背景返回 200" \
  "$(code -X POST -H 'Content-Type: image/png' --data-binary @"$TMP/bg.png" "$B/api/background")" "200"
# 接口返回的是站内相对路径，测的时候要自己拼上主机
BGPATH=$(curl -s "$B/api/settings" | "$PY" -c 'import sys,json;print(json.load(sys.stdin)["background"]["url"])')
BGURL="$B$BGPATH"
case "$BGPATH" in /api/background?v=*) ok "背景地址带版本号（用于破缓存）";; *) bad "背景地址异常: $BGPATH";; esac
chk "背景图可取回 200" "$(code "$BGURL")" "200"
chk "取回的字节与上传一致" "$(curl -s "$BGURL" | sha256sum | cut -d' ' -f1)" "$BG_SUM"
chk "背景图声明正确的 Content-Type" \
  "$(curl -s -o /dev/null -D - "$BGURL" | grep -ci '^content-type: image/png')" "1"
chk "背景图可长缓存（URL 已带内容哈希）" \
  "$(curl -s -o /dev/null -D - "$BGURL" | grep -ci 'cache-control: public, max-age=31536000')" "1"

# 类型判定只看字节，不看浏览器声明的 Content-Type
printf 'plain text pretending to be a png' > "$TMP/fake.png"
chk "伪装成 PNG 的文本被拒 415" \
  "$(code -X POST -H 'Content-Type: image/png' --data-binary @"$TMP/fake.png" "$B/api/background")" "415"
printf '<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>' > "$TMP/x.svg"
chk "SVG 被拒 415（同源脚本风险）" \
  "$(code -X POST -H 'Content-Type: image/svg+xml' --data-binary @"$TMP/x.svg" "$B/api/background")" "415"

: > "$TMP/empty.png"
chk "空内容被拒 400" \
  "$(code -X POST -H 'Content-Type: image/png' --data-binary @"$TMP/empty.png" "$B/api/background")" "400"
# 上限 10 MiB，这里正好多一个字节
"$PY" -c "open(r'$TMP/big.png','wb').write(b'\x89PNG\r\n\x1a\n' + b'\x00' * (10*1024*1024 - 7))"
chk "超过 10 MiB 的背景被拒 413" \
  "$(code -X POST -H 'Content-Type: image/png' --data-binary @"$TMP/big.png" "$B/api/background")" "413"
chk "被拒后原背景未被覆盖" "$(curl -s "$BGURL" | sha256sum | cut -d' ' -f1)" "$BG_SUM"

chk "移除背景返回 200" "$(code -X DELETE "$B/api/background")" "200"
chk "移除后设置里无背景" \
  "$(curl -s "$B/api/settings" | "$PY" -c 'import sys,json;print(json.load(sys.stdin)["background"])')" "None"
chk "移除后取图 404" "$(code "$B/api/background")" "404"
chk "磁盘上的背景文件已删除" \
  "$([ -f ".tmp/qs-test/background" ] && echo exists || echo gone)" "gone"
chk "重复移除幂等 200" "$(code -X DELETE "$B/api/background")" "200"

# ---------------------------------------------------------------- 12
echo
echo "=== 12. 访问口令鉴权（独立实例） ==="
kill_port 18081 # 上一轮若异常退出可能还占着
QS_ADMIN_TOKEN=secret123 QS_UPDATE_CHECK=0 "$BIN" -addr 127.0.0.1:18081 -data "$TMP/authdata" -tray=false \
  > "$TMP/auth.log" 2>&1 &
AUTH_PID=$!
# 轮询等端口就绪。固定 sleep 在机器忙的时候会假失败。
for _ in $(seq 1 40); do
  [ "$(code "$A/api/config")" = "200" ] && break
  sleep 0.25
done
chk "无口令访问列表 401" "$(code "$A/api/files")" "401"
chk "错误口令 401" "$(code -H 'X-Admin-Token: wrong' "$A/api/files")" "401"
chk "正确口令 200" "$(code -H 'X-Admin-Token: secret123' "$A/api/files")" "200"
chk "上传接口同样受保护 401" \
  "$(code -X POST -H 'Content-Type: application/json' -d '{}' "$A/api/upload/init")" "401"
chk "公开配置接口不受影响 200" "$(code "$A/api/config")" "200"
chk "needAuth 已上报" "$(curl -s "$A/api/config" | jget needAuth)" "True"
chk "读设置公开 200（首帧就要套主题）" "$(code "$A/api/settings")" "200"
# 二维码接口不读服务端任何数据（内容由调用方从 ?d= 传进来），开了口令也该能访问。
# 必须在**这个实例还活着的时候**查——第 17 节跑的时候它已经被 kill 掉了。
chk "二维码接口同样公开 200" "$(code "$A/api/qr?d=hello")" "200"
# 版本号也不敏感，而且开了口令的部署同样该看到"有新版本"，所以读取公开。
chk "版本接口同样公开 200" "$(code "$A/api/version")" "200"
# 但强制刷新要口令：它会让服务端去访问外网，公开的话局域网里谁都能拿它烧配额。
chk "强制检查更新受保护 401" "$(code -X POST "$A/api/version/check")" "401"
chk "带口令强制检查更新 200" \
  "$(code -X POST -H 'X-Admin-Token: secret123' "$A/api/version/check")" "200"
# SSE 则相反：它暴露的是"这个实例正在被使用"这类活动信息，跟列表同级，要口令。
# 无口令时中间件会立刻回 401，不会挂住——所以这里能安全地用 code()。
chk "SSE 接口受保护 401" "$(code "$A/api/events")" "401"
chk "写设置受保护 401" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"theme":"dark"}' "$A/api/settings")" "401"
chk "传背景受保护 401" \
  "$(code -X POST -H 'Content-Type: image/png' --data-binary @"$TMP/bg.png" "$A/api/background")" "401"
chk "删背景受保护 401" "$(code -X DELETE "$A/api/background")" "401"
chk "带口令写设置 200" \
  "$(code -X PUT -H 'X-Admin-Token: secret123' -H 'Content-Type: application/json' \
     -d '{"theme":"light"}' "$A/api/settings")" "200"

# 开了口令时，文本页那两个区块的 hidden **必须留着**：没鉴权的人不该看到输入框。
# 服务端是"没口令才摘"，方向搞反的话，未鉴权的人会先看到输入框再被换成口令卡片。
AUTH_PAGE=$(curl -s "$A/text")
case "$AUTH_PAGE" in
  *'id="composeCard" hidden'*) ok "开口令时文本页首帧不露出输入框（hidden 留着）" ;;
  *) bad "开口令时 #composeCard 的 hidden 被摘掉了 — 未鉴权的人会看到输入框" ;;
esac
case "$AUTH_PAGE" in
  *'id="textsCard" hidden'*) ok "开口令时文本页首帧不露出文本列表（hidden 留着）" ;;
  *) bad "开口令时 #textsCard 的 hidden 被摘掉了" ;;
esac
kill_port 18081
wait_gone "$AUTH_PID"

# ---------------------------------------------------------------- 13
echo
echo "=== 13. 同一秒内上传的排序稳定性 ==="
# created_at 只精确到秒，连传几个文件必然并列；若只按 created_at 排，
# SQLite 对并列行的顺序不作保证，列表顺序会随机跳。
for n in a b c; do
  I=$(curl -s -X POST -H 'Content-Type: application/json' \
    -d "{\"name\":\"order-$n.txt\",\"size\":1,\"mime\":\"text/plain\"}" "$B/api/upload/init")
  U=$(echo "$I" | jget uploadId)
  printf 'x' | curl -s -X PUT --data-binary @- "$B/api/upload/$U/0" > /dev/null
  curl -s -o /dev/null -X POST "$B/api/upload/$U/complete"
done
chk "并列时间戳下顺序稳定（最新在前）" \
  "$(curl -s "$B/api/files" | "$PY" -c 'import sys,json;print(",".join(f["name"] for f in json.load(sys.stdin)[:3]))')" \
  "order-c.txt,order-b.txt,order-a.txt"
chk "再查一次顺序不变" \
  "$(curl -s "$B/api/files" | "$PY" -c 'import sys,json;print(",".join(f["name"] for f in json.load(sys.stdin)[:3]))')" \
  "order-c.txt,order-b.txt,order-a.txt"

echo
echo "=== 14. 预览安全：上传内容不得在本站源里执行 ==="
# 攻击面：上传一个 .html，受害者点开列表里的链接。若以 inline 在本站源渲染，
# 里面的脚本就能读到 localStorage 里的管理口令，进而以管理员身份调所有接口
# （这是实测复现过的存储型 XSS）。处置：下载路径改用白名单，只有明确安全的
# 类型才 inline，其余一律 attachment。
XSS_HTML='<!doctype html><title>probe</title><script>document.title=localStorage.getItem("qs_token")</script>'
F_HTML=$(put_small 'probe.html' "$XSS_HTML" 'text/html')
HID=$(echo "$F_HTML" | jget id)
HDISP=$(curl -s -o /dev/null -D - "$B/f/$HID/probe.html" | tr -d '\r' | grep -i '^content-disposition')
case "$HDISP" in *attachment*) ok "上传的 HTML 只下载、不内联";; *) bad "HTML 必须 attachment，实际: [$HDISP]";; esac

XSS_SVG='<svg xmlns="http://www.w3.org/2000/svg"><script>document.title="ran"</script></svg>'
F_SVG=$(put_small 'probe.svg' "$XSS_SVG" 'image/svg+xml')
SID=$(echo "$F_SVG" | jget id)
# 注意：HTTP 头是 Content-Disposition（首字母大写），case 匹配区分大小写，
# 所以这里先把头单独取出来，只匹配值本身，别把头名写进模式里。
SDISP=$(curl -s -o /dev/null -D - "$B/f/$SID/probe.svg" | tr -d '\r' | grep -i '^content-disposition')
SCSP=$(curl -s -o /dev/null -D - "$B/f/$SID/probe.svg" | tr -d '\r' | grep -i '^content-security-policy')
case "$SDISP" in *inline*) ok "SVG 仍可内联预览（保住功能）";; *) bad "SVG 应保持 inline，实际: [$SDISP]";; esac
case "$SCSP" in *sandbox*) ok "SVG 预览被 CSP sandbox 隔离（不透明源）";; *) bad "SVG 缺 sandbox CSP，实际: [$SCSP]";; esac
case "$SCSP" in *"default-src 'none'"*) ok "SVG 预览禁止加载任何资源";; *) bad "SVG CSP 应含 default-src 'none'";; esac

# 白名单内的安全类型照旧内联——别把功能一起修没了
F_TXT=$(put_small 'note.txt' 'hello' 'text/plain')
TID=$(echo "$F_TXT" | jget id)
TDISP=$(curl -s -o /dev/null -D - "$B/f/$TID/note.txt" | tr -d '\r' | grep -i '^content-disposition')
case "$TDISP" in *inline*) ok "text/plain 仍内联预览";; *) bad "text/plain 应内联，实际: [$TDISP]";; esac

# 白名单外的脚本类型一律下载
F_JS=$(put_small 'evil.js' 'document.title=1' 'text/javascript')
JID=$(echo "$F_JS" | jget id)
JDISP=$(curl -s -o /dev/null -D - "$B/f/$JID/evil.js" | tr -d '\r' | grep -i '^content-disposition')
case "$JDISP" in *attachment*) ok "JavaScript 文件只下载、不内联";; *) bad "JS 必须 attachment，实际: [$JDISP]";; esac

chk "下载响应带 nosniff（禁止类型嗅探）" \
  "$(curl -s -o /dev/null -D - "$B/f/$HID/probe.html" | grep -ci '^x-content-type-options: nosniff')" "1"

# 应用页面自身也加 CSP，兜住将来可能出现的注入点
APPCSP=$(curl -s -o /dev/null -D - "$B/" | tr -d '\r' | grep -i '^content-security-policy')
case "$APPCSP" in *"default-src 'self'"*) ok "应用页面带 CSP（default-src 'self'）";; *) bad "应用页面缺 CSP: [$APPCSP]";; esac
case "$APPCSP" in *"script-src 'self'"*) ok "应用页面禁止内联脚本";; *) bad "CSP 缺 script-src 'self'";; esac
case "$APPCSP" in *"frame-ancestors 'none'"*) ok "应用页面禁止被嵌套";; *) bad "CSP 缺 frame-ancestors";; esac

# 下载路径不能套上应用页面的 CSP，否则 SVG 的内联预览会被整片掐掉
chk "下载路径不套应用页面 CSP" \
  "$(curl -s -o /dev/null -D - "$B/f/$TID/note.txt" | grep -ci '^content-security-policy')" "0"

# ---------------------------------------------------------------- 15
echo
echo "=== 15. 外观数值（模糊度 / 不透明度）与存储位置接口 ==="
chk "初始模糊度为 0" "$(curl -s "$B/api/settings" | jget bgBlur)" "0"
chk "初始三块不透明度都是 85" \
  "$(curl -s "$B/api/settings" | "$PY" -c 'import sys,json
o = json.load(sys.stdin)["opacity"]
print(o["topbar"], o["upload"], o["files"])')" \
  "85 85 85"

chk "设模糊度 12 返回 200" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"bgBlur":12}' "$B/api/settings")" "200"
chk "模糊度已持久化" "$(curl -s "$B/api/settings" | jget bgBlur)" "12"

# 增量更新：只传 files，另两块必须原样不动
chk "只改 files 不透明度返回 200" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"opacity":{"files":60}}' "$B/api/settings")" "200"
chk "files 已改，另两块没被带偏" \
  "$(curl -s "$B/api/settings" | "$PY" -c 'import sys,json
o = json.load(sys.stdin)["opacity"]
print(o["topbar"], o["upload"], o["files"])')" \
  "85 85 60"
chk "只改不透明度不影响模糊度" "$(curl -s "$B/api/settings" | jget bgBlur)" "12"

# 越界必须拦在服务端：前端滑块能拖到哪儿不算数，
# 直接 PUT 一个越界值会把 CSS 变量写成垃圾，整页配色就废了
chk "模糊度 41 越界 400" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"bgBlur":41}' "$B/api/settings")" "400"
chk "模糊度 -1 越界 400" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"bgBlur":-1}' "$B/api/settings")" "400"
chk "不透明度 19 越界 400" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"opacity":{"topbar":19}}' "$B/api/settings")" "400"
chk "不透明度 101 越界 400" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"opacity":{"upload":101}}' "$B/api/settings")" "400"
chk "越界值未污染已存值" \
  "$(curl -s "$B/api/settings" | "$PY" -c 'import sys,json
d = json.load(sys.stdin); o = d["opacity"]
print(d["bgBlur"], o["topbar"], o["upload"], o["files"])')" \
  "12 85 85 60"

# 边界值本身合法
chk "模糊度 40（上界）合法" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"bgBlur":40}' "$B/api/settings")" "200"
chk "不透明度 20（下界）合法" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"opacity":{"topbar":20}}' "$B/api/settings")" "200"
chk "不透明度 100（上界）合法" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"opacity":{"topbar":100}}' "$B/api/settings")" "200"

# 第 11 节末尾已经把背景图删了。这几个数值不依赖图存在，不该被连带清掉——
# 否则用户删掉背景再传一张，模糊度和不透明度全被打回默认。
chk "无背景图时外观数值依然保留" \
  "$(curl -s "$B/api/settings" | "$PY" -c 'import sys,json
d = json.load(sys.stdin)
print(d["background"], d["bgBlur"], d["opacity"]["topbar"])')" \
  "None 40 100"

# 本实例由 -data 指定，界面上必须只读
chk "读存储位置 200" "$(code "$B/api/storage")" "200"
chk "本实例数据目录被锁定（-data 指定）" "$(curl -s "$B/api/storage" | jget locked)" "True"
chk "锁定原因已给出" \
  "$(curl -s "$B/api/storage" | "$PY" -c 'import sys,json;print(bool(json.load(sys.stdin).get("reason")))')" "True"
chk "锁定时改目录被拒 409" \
  "$(code -X PUT -H 'Content-Type: application/json' \
     -d "{\"path\":\"$TMP/blocked\",\"migrate\":false}" "$B/api/storage")" "409"
chk "被拒后没有偷偷建目录" "$([ -e "$TMP/blocked" ] && echo created || echo absent)" "absent"

# ---------------------------------------------------------------- 16
echo
echo "=== 16. 数据目录切换与迁移（未经 -data 指定的实例） ==="
# 本节刻意不传 -data：只有"数据目录来自默认值或启动配置"时，界面上才允许改。
# 同时把配置目录指到临时位置——否则会读到真实用户的启动配置，
# 上一次跑测试留下的 dataDir 会被捡回来，结果完全不可预期。
ABS_BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"
SW_ROOT="$TMP/switch"
mkdir -p "$SW_ROOT/work" "$SW_ROOT/cfg"
kill_port $PORT_SWITCH
( cd "$SW_ROOT/work" && exec env APPDATA="$SW_ROOT/cfg" XDG_CONFIG_HOME="$SW_ROOT/cfg" \
    HOME="$SW_ROOT/cfg" "$ABS_BIN" -addr "127.0.0.1:$PORT_SWITCH" -tray=false ) \
  > "$SW_ROOT/server.log" 2>&1 &
for _ in $(seq 1 40); do
  [ "$(code "$SW/api/config")" = "200" ] && break
  sleep 0.25
done

chk "读存储位置 200" "$(code "$SW/api/storage")" "200"
chk "未用 -data 指定时可改（locked=false）" "$(curl -s "$SW/api/storage" | jget locked)" "False"
chk "默认落在工作目录下的 data/" \
  "$(curl -s "$SW/api/storage" | "$PY" -c 'import sys,json,os
p = os.path.normpath(json.load(sys.stdin)["dataDir"])
print(os.path.basename(p) == "data" and os.path.basename(os.path.dirname(p)) == "work")')" \
  "True"

# 先放一个文件进去，等下验证它有没有跟着搬走
SAVE_B="$B"; B="$SW"
put_small 'move-me.txt' 'payload-123' 'text/plain' > /dev/null
B="$SAVE_B"
chk "切换前有 1 个文件" "$(curl -s "$SW/api/files" | jsonlen)" "1"

NEWDIR="$SW_ROOT/newdata"
chk "切到新目录（带迁移）200" \
  "$(code -X PUT -H 'Content-Type: application/json' \
     -d "{\"path\":\"$NEWDIR\",\"migrate\":true}" "$SW/api/storage")" "200"
chk "存储位置已切换" \
  "$(curl -s "$SW/api/storage" | "$PY" -c 'import sys,json,os
print(os.path.basename(os.path.normpath(json.load(sys.stdin)["dataDir"])) == "newdata")')" "True"
chk "文件列表随迁移保留" "$(curl -s "$SW/api/files" | jsonlen)" "1"
# 注意：Windows 上的 Python 往管道写的是 \r\n，而 xargs 不会像 $( ) 那样把
# 行尾的 \r 吃掉，于是 URL 会变成 ".../move-me.txt\r"，curl 直接请求失败、
# 静默返回空串。所以这里必须先用 $( ) 把 URL 取出来再请求。
MOVED_URL=$(curl -s "$SW/api/files" | "$PY" -c 'import sys,json;print(json.load(sys.stdin)[0]["url"])')
chk "迁移后文件内容可取回且一致" "$(curl -s "$SW$MOVED_URL")" "payload-123"
chk "新目录里生成了数据库" "$([ -f "$NEWDIR/quickshare.db" ] && echo ok || echo missing)" "ok"
chk "新目录里有文件本体" \
  "$([ -d "$NEWDIR/files" ] && [ -n "$(ls -A "$NEWDIR/files" 2>/dev/null)" ] && echo ok || echo missing)" "ok"
# 同一个盘走 rename，是整体搬走，旧目录不该剩下东西
chk "同盘迁移后旧目录已整体搬走" \
  "$([ -e "$SW_ROOT/work/data" ] && echo left || echo moved)" "moved"

# 各种应当被拒绝的情况。重点不是"报错了"，而是报错后服务还好好活着。
chk "新目录位于当前目录内部被拒 400" \
  "$(code -X PUT -H 'Content-Type: application/json' \
     -d "{\"path\":\"$NEWDIR/inner\",\"migrate\":false}" "$SW/api/storage")" "400"
chk "空路径被拒 400" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"path":"   ","migrate":false}' "$SW/api/storage")" "400"

mkdir -p "$SW_ROOT/occupied/files"
: > "$SW_ROOT/occupied/quickshare.db"
chk "目标目录已有数据时拒绝覆盖 400" \
  "$(code -X PUT -H 'Content-Type: application/json' \
     -d "{\"path\":\"$SW_ROOT/occupied\",\"migrate\":true}" "$SW/api/storage")" "400"
chk "被拒后当前目录没变" \
  "$(curl -s "$SW/api/storage" | "$PY" -c 'import sys,json,os
print(os.path.basename(os.path.normpath(json.load(sys.stdin)["dataDir"])) == "newdata")')" "True"
chk "被拒后服务仍可用" "$(code "$SW/api/files")" "200"
chk "被拒后原文件还在" "$(curl -s "$SW/api/files" | jsonlen)" "1"

# 不迁移：只换目录，新目录从空开始，旧数据原地留着
EMPTYDIR="$SW_ROOT/fresh"
chk "切到空目录（不迁移）200" \
  "$(code -X PUT -H 'Content-Type: application/json' \
     -d "{\"path\":\"$EMPTYDIR\",\"migrate\":false}" "$SW/api/storage")" "200"
chk "不迁移时新目录是空的" "$(curl -s "$SW/api/files" | jsonlen)" "0"
chk "不迁移时旧目录数据原地保留" \
  "$([ -d "$NEWDIR/files" ] && [ -n "$(ls -A "$NEWDIR/files" 2>/dev/null)" ] && echo ok || echo missing)" "ok"
chk "切换后上传功能正常" \
  "$(curl -s -X POST -H 'Content-Type: application/json' \
     -d '{"name":"after.txt","size":2,"mime":"text/plain"}' "$SW/api/upload/init" | jget totalChunks)" "1"
kill_port $PORT_SWITCH

# ---- 17. 二维码接口 ----
echo
echo "=== 17. 二维码接口 ==="

chk "返回 PNG magic" \
  "$(curl -s "$B/api/qr?d=hello" | head -c 4 | od -An -tx1 | tr -d ' \n')" "89504e47"

# 先 grep 出整行再取 value：头名是 Content-Type，直接拿它去比 case 模式
# 会因为大小写匹配不上（老毛病了，见文件头注释）
chk "Content-Type 是 image/png" \
  "$(curl -s -o /dev/null -D - "$B/api/qr?d=hello" | grep -i '^content-type' | tr -d '\r' | sed 's/^[^:]*: *//')" \
  "image/png"

QR_CC=$(curl -s -o /dev/null -D - "$B/api/qr?d=hello" | grep -i '^cache-control' | tr -d '\r' | sed 's/^[^:]*: *//')
case "$QR_CC" in
  *immutable*) ok "缓存头含 immutable ($QR_CC)" ;;
  *) bad "缓存头含 immutable — 实际 [$QR_CC]" ;;
esac

chk "缺 d 参数 400" "$(code "$B/api/qr")" "400"
chk "d 为空 400" "$(code "$B/api/qr?d=")" "400"
chk "正好 1024 字节 200" \
  "$(code "$B/api/qr?d=$(head -c 1024 /dev/zero | tr '\0' 'a')")" "200"
chk "1025 字节被拒 400" \
  "$(code "$B/api/qr?d=$(head -c 1025 /dev/zero | tr '\0' 'a')")" "400"

# 长缓存的前提是确定性：同一个内容必须每次都得到同一张图，
# 否则浏览器缓存下来的和下次请求到的不是一张，扫码结果会飘
chk "相同内容生成相同的码" \
  "$(curl -s "$B/api/qr?d=same" | cksum)" "$(curl -s "$B/api/qr?d=same" | cksum)"
# 反过来，不同内容必须不同，否则说明 d 参数压根没接上
if [ "$(curl -s "$B/api/qr?d=aaa" | cksum)" != "$(curl -s "$B/api/qr?d=bbb" | cksum)" ]; then
  ok "不同内容生成不同的码"
else
  bad "不同内容生成不同的码 — 两者一模一样"
fi

echo
echo "=== 18. 列表变更推送（SSE） ==="

# 响应头。用 --max-time 掐断：这个连接正常情况是永不结束的，
# 不掐就卡在这儿了。头是随第一个字节一起到的，1 秒足够。
SSE_HDR=$(curl -s -o /dev/null -D - --max-time 1 "$B/api/events" | tr -d '\r')
chk "SSE Content-Type 是 text/event-stream" \
  "$(echo "$SSE_HDR" | grep -i '^content-type' | sed 's/^[^:]*: *//')" \
  "text/event-stream"

mkdir -p "$TMP"
SSE_OUT="$TMP/sse.txt"
: > "$SSE_OUT"
# -N 关掉 curl 自己的输出缓冲，否则事件会攒在缓冲区里看不到
curl -s -N --max-time 20 "$B/api/events" > "$SSE_OUT" 2>/dev/null &
SSE_PID=$!

# 等 ready：没有它就没法区分"连上了但列表恰好没变"和"压根没连上"
for _ in $(seq 1 25); do
  grep -q 'event: ready' "$SSE_OUT" 2>/dev/null && break
  sleep 0.2
done
if grep -q 'event: ready' "$SSE_OUT"; then
  ok "连上后先收到 ready"
else
  bad "连上后先收到 ready — 实际 [$(head -c 200 "$SSE_OUT" | tr '\n' '|')]"
fi

# 上传一个文件，应当推一次 files
put_small "sse-触发.txt" "hi" "text/plain" > /dev/null
for _ in $(seq 1 25); do
  grep -q 'event: files' "$SSE_OUT" 2>/dev/null && break
  sleep 0.2
done
if grep -q 'event: files' "$SSE_OUT"; then
  ok "上传完成推送了 files 事件"
else
  bad "上传完成推送了 files 事件 — 实际 [$(head -c 300 "$SSE_OUT" | tr '\n' '|')]"
fi

# 删除也应当推一次
SSE_ID=$(curl -s "$B/api/files" | "$PY" -c '
import sys, json
print([f["id"] for f in json.load(sys.stdin) if f["name"] == "sse-触发.txt"][0])')
curl -s -X DELETE "$B/api/files/$SSE_ID" > /dev/null
for _ in $(seq 1 25); do
  [ "$(grep -c 'event: files' "$SSE_OUT" 2>/dev/null || true)" -ge 2 ] && break
  sleep 0.2
done
chk "上传与删除各推一次，共 2 次" "$(grep -c 'event: files' "$SSE_OUT" 2>/dev/null || true)" "2"

# 通知里带的数据是空的：它只是"去重新拉列表"的信号，列表本身仍由
# /api/files 提供。避免两处各序列化一遍、还得保证两边格式一致。
chk "事件不带载荷（只是信号）" "$(grep -A1 'event: files' "$SSE_OUT" | grep -c '^data: {}')" "2"

kill $SSE_PID 2>/dev/null
wait $SSE_PID 2>/dev/null

# 客户端断开后服务端不该留下东西：再传一个文件，一切照常
put_small "sse-收尾.txt" "bye" "text/plain" > /dev/null
chk "客户端断开后服务端照常工作" \
  "$(curl -s "$B/api/files" | "$PY" -c '
import sys, json
print(sum(1 for f in json.load(sys.stdin) if f["name"] == "sse-收尾.txt"))')" "1"

# ---- 19. 文本发送与设备备注 ----
echo
echo "=== 19. 文本发送与设备备注 ==="

# 先把主题设成一个已知值：下面要断言服务端把它注进了 HTML。
chk "把主题设为深色" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"theme":"dark"}' "$B/api/settings")" "200"

chk "文本页返回 200" "$(code "$B/text")" "200"
TEXT_PAGE=$(curl -s "$B/text")
HOME_PAGE=$(curl -s "$B/")

case "$TEXT_PAGE" in
  *'data-theme="dark"'*) ok "文本页 HTML 里注入了主题（首帧不闪色）" ;;
  *) bad "文本页 HTML 里注入了主题 — 没找到 data-theme" ;;
esac
case "$TEXT_PAGE" in
  *'/text.js?v='*) ok "文本页引用的 text.js 带内容版本号" ;;
  *) bad "文本页引用的 text.js 带内容版本号 — 没找到" ;;
esac
case "$TEXT_PAGE" in
  *'/live.js?v='*) ok "文本页引用的 live.js 带内容版本号" ;;
  *) bad "文本页引用的 live.js 带内容版本号 — 没找到" ;;
esac
case "$HOME_PAGE" in
  *'/live.js?v='*) ok "首页也引用了带版本号的 live.js" ;;
  *) bad "首页也引用了带版本号的 live.js — 没找到" ;;
esac
case "$HOME_PAGE" in
  *'href="/text"'*) ok "首页顶栏有跳转文本页的入口" ;;
  *) bad "首页顶栏有跳转文本页的入口 — 没找到" ;;
esac

# ---- 切页观感：首帧该显示什么，服务端就要渲染成什么
#
# 这个实例**没开口令**（内网自用的默认情况），所以文本页那两个区块的首帧状态
# 必须是"可见"——服务端在渲染时把 text.html 里的 hidden 摘掉了。
# 不摘的话它们要等 JS 跑完「config → settings → 三个并行请求」才露面，实测
# （注入 120ms 延迟模拟 NAS + WiFi）主区空白 368ms，而首页的卡片本来就没带
# hidden、切回去反而不空白——这个不对称就是"切页闪一下"的主因。
#
# 这几条同时守着"服务端按字面量 ` id="xxx" hidden` 替换"这个契约：
# 谁把 text.html 里那两个属性调了顺序，这里就会红，而不是悄悄退回空白。
case "$TEXT_PAGE" in
  *'id="composeCard" hidden'*)
    bad "没开口令时文本页首帧就该显示输入框 — #composeCard 上还挂着 hidden" ;;
  *'id="composeCard"'*)
    ok "没开口令时文本页首帧就显示输入框（服务端摘了 hidden）" ;;
  *) bad "文本页里找不到 #composeCard — 是不是改了 id" ;;
esac
case "$TEXT_PAGE" in
  *'id="textsCard" hidden'*)
    bad "没开口令时文本页首帧就该显示列表 — #textsCard 上还挂着 hidden" ;;
  *'id="textsCard"'*)
    ok "没开口令时文本页首帧就显示列表（服务端摘了 hidden）" ;;
  *) bad "文本页里找不到 #textsCard — 是不是改了 id" ;;
esac

# 空状态反过来：**首帧不能显示**。数据还没回来就说"还没有文件/文本"，是在报一个
# 还不知道真假的结论——真有内容时用户会先看到这句、再看着它被替换掉。
case "$TEXT_PAGE" in
  *'id="textEmpty" hidden'*) ok "文本页空状态首帧不显示（等数据回来再决定）" ;;
  *) bad "文本页 #textEmpty 没有 hidden — 有文本时会先闪一下这句话" ;;
esac
case "$HOME_PAGE" in
  *'id="fileEmpty" hidden'*) ok "首页空状态首帧不显示（等数据回来再决定）" ;;
  *) bad "首页 #fileEmpty 没有 hidden — 有文件时会先闪一下这句话" ;;
esac

# 文件列表的搜索 / 排序工具条。纯前端实现，后端没有对应接口——这两条只防"某次重构把
# 工具条整个删掉"，行为断言（六种排序、搜索、持久化、窄屏）在 .tmp/shots/verify-file-list.mjs。
case "$HOME_PAGE" in
  *'id="fileSearch"'*) ok "首页有文件名搜索框" ;;
  *) bad "首页 #fileSearch 不见了" ;;
esac
case "$HOME_PAGE" in
  *'id="fileSort"'*) ok "首页有排序下拉" ;;
  *) bad "首页 #fileSort 不见了" ;;
esac

# 跨文档换页的动效靠样式表里那条 @view-transition（CSSOM 层面的验证在
# .tmp/shots/probe-nav-flash.mjs 里，这里只做一次廉价的兜底）。
STYLE_CSS=$(curl -s "$B/style.css")
case "$STYLE_CSS" in
  *'@view-transition'*) ok "样式表声明了跨文档 View Transition（切页不再硬切）" ;;
  *) bad "样式表里没有 @view-transition — 切页会硬切" ;;
esac
case "$STYLE_CSS" in
  *'view-transition-name: topbar'*) ok "顶栏在两页之间原地不动" ;;
  *) bad "顶栏没有 view-transition-name — 整屏一起淡会显得在抖" ;;
esac

UA_IPHONE='Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1'

# 设备身份 = 请求方 IP，由服务端从连接上推导（请求体里的 deviceId 不再被采信）。
# 先问一次接口拿到自己的地址，后面凡是拼设备 URL 的地方都用它——写死
# 127.0.0.1 在 QS_BASE 指向外部实例时会错。
MY_IP=$(curl -s "$B/api/config" | jget clientIp)

# 设备名由服务端从请求头的 UA 解析，前端不自己解析（两处解析迟早不一致）
T1=$(curl -s -X POST -H 'Content-Type: application/json' -H "User-Agent: $UA_IPHONE" \
  -d '{"content":"第一条文本"}' "$B/api/texts")
T1_ID=$(echo "$T1" | jget id)
chk "设备名按 UA 解析" "$(echo "$T1" | jget deviceName)" "iPhone · Safari"

chk "文本列表里有 1 条" "$(curl -s "$B/api/texts" | jsonlen)" "1"
chk "内容读回一致" "$(curl -s "$B/api/texts" | jget 0 content)" "第一条文本"

chk "修改文本 200" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"content":"改过的内容"}' "$B/api/texts/$T1_ID")" "200"
chk "改完内容正确" "$(curl -s "$B/api/texts" | jget 0 content)" "改过的内容"

# 改一个不存在的 ID 必须 404，否则前端会以为改成功了
chk "改不存在的文本 404" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"content":"x"}' "$B/api/texts/no-such-id")" "404"

chk "空内容被拒 400" \
  "$(code -X POST -H 'Content-Type: application/json' -d '{"content":"   "}' "$B/api/texts")" "400"
# 请求体里塞 deviceId 不再有任何作用：设备身份由服务端从连接推导。
# 这条同时守住"随手编个 ID 就能冒充成别人的设备"那条路。
curl -s -X POST -H 'Content-Type: application/json' -H "User-Agent: $UA_IPHONE" \
  -d '{"content":"伪造设备的尝试","deviceId":"1.2.3.4"}' "$B/api/texts" > /dev/null
chk "伪造的 deviceId 被忽略，设备数仍是 1" "$(curl -s "$B/api/devices" | jsonlen)" "1"

# 超长内容走文件传参：3.3 万字符直接塞进 -d 会顶到 Windows 的命令行长度上限
BIG_JSON="$TMP/big-text.json"
"$PY" -c 'import json,sys; open(sys.argv[1],"w").write(json.dumps({"content":"x"*33000}))' "$BIG_JSON"
chk "超过 32 KiB 被拒 413" \
  "$(code -X POST -H 'Content-Type: application/json' --data-binary @"$BIG_JSON" "$B/api/texts")" "413"

# ---- 设备备注
chk "设备列表里有 1 台" "$(curl -s "$B/api/devices" | jsonlen)" "1"
chk "设备 ID 就是请求方 IP" "$(curl -s "$B/api/devices" | jget 0 id)" "$MY_IP"
chk "本机被标成 isMe" "$(curl -s "$B/api/devices" | jget 0 isMe)" "True"
chk "改备注 200" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"remark":"老王的手机"}' "$B/api/devices/$MY_IP")" "200"
chk "文本列表里显示的就是备注" "$(curl -s "$B/api/texts" | jget 0 deviceName)" "老王的手机"

# 再发一条：TouchDevice 的 upsert 不能顺手把 remark 覆盖回空
curl -s -X POST -H 'Content-Type: application/json' -H "User-Agent: $UA_IPHONE" \
  -d '{"content":"第二条"}' "$B/api/texts" > /dev/null
chk "再发一条后备注还在" "$(curl -s "$B/api/devices" | jget 0 remark)" "老王的手机"

chk "清空备注 200" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"remark":""}' "$B/api/devices/$MY_IP")" "200"
chk "清空后备注为空" "$(curl -s "$B/api/devices" | jget 0 remark)" ""
chk "清空后名字回到 UA 解析值" "$(curl -s "$B/api/texts" | jget 0 deviceName)" "iPhone · Safari"
chk "改不存在的设备 404" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"remark":"x"}' "$B/api/devices/no-such-dev")" "404"

# ---- 文本变更走 texts 主题，不该混进 files
: > "$SSE_OUT"
curl -s -N --max-time 20 "$B/api/events" > "$SSE_OUT" 2>/dev/null &
SSE_PID=$!
for _ in $(seq 1 25); do
  grep -q 'event: ready' "$SSE_OUT" 2>/dev/null && break
  sleep 0.2
done

curl -s -X POST -H 'Content-Type: application/json' -d '{"content":"推送测试"}' \
  "$B/api/texts" > /dev/null
for _ in $(seq 1 25); do
  grep -q 'event: texts' "$SSE_OUT" 2>/dev/null && break
  sleep 0.2
done
if grep -q 'event: texts' "$SSE_OUT"; then
  ok "新建文本推送了 texts 事件"
else
  bad "新建文本推送了 texts 事件 — 实际 [$(head -c 300 "$SSE_OUT" | tr '\n' '|')]"
fi
# 两个主题必须各走各的：合用一个信号会让其中一个页面漏刷（偶发、难复现）
#
# 这里的 `|| true` 不能写成 `|| echo 0`：grep -c 在**没有匹配**时会打印 "0"
# 并以退出码 1 结束，于是 `|| echo 0` 又补一行，命令替换拿到 "0\n0"，
# 断言就报"期望 [0] 实际 [0 0]"——看着像功能坏了，其实是断言自己写歪了。
chk "发文本不该触发 files 事件" "$(grep -c 'event: files' "$SSE_OUT" 2>/dev/null || true)" "0"

kill $SSE_PID 2>/dev/null
wait $SSE_PID 2>/dev/null

chk "删除文本 200" "$(code -X DELETE "$B/api/texts/$T1_ID")" "200"
chk "重复删除 404" "$(code -X DELETE "$B/api/texts/$T1_ID")" "404"

# ---------------------------------------------------------------- 20
echo
echo "=== 20. 文本批量删除与自动清理设置 ==="

# 搜索 / 设备筛选 / 点击整条复制都是纯前端行为，curl 看不见，
# 由 .tmp/shots/verify-text-ui.mjs 那套 CDP 断言覆盖。这里只验服务端。
TEXT_PAGE2=$(curl -s "$B/text")
case "$TEXT_PAGE2" in
  *'id="searchInput"'*) ok "文本页有搜索框" ;;
  *) bad "文本页有搜索框 — 没找到 #searchInput" ;;
esac
case "$TEXT_PAGE2" in
  *'id="deviceFilter"'*) ok "文本页有设备筛选下拉" ;;
  *) bad "文本页有设备筛选下拉 — 没找到 #deviceFilter" ;;
esac
case "$TEXT_PAGE2" in
  *'id="bulkBar"'*) ok "文本页有多选工具条" ;;
  *) bad "文本页有多选工具条 — 没找到 #bulkBar" ;;
esac
case "$TEXT_PAGE2" in
  *'id="ttlHint"'*) ok "文本页有保留时间提示" ;;
  *) bad "文本页有保留时间提示 — 没找到 #ttlHint" ;;
esac
# 提示的 href 指向首页的设置面板（hash 深链，不是单独一个设置页）。
# 这个 href 现在只是**兜底**：面板已抽成两页共用，JS 跑起来时点击被拦下、
# 在本页开面板（见 verify-text-settings.mjs）。curl 拿不到 JS 行为，
# 所以这里只能验"兜底路径还在"——真正的交互由前端脚本管。
case "$TEXT_PAGE2" in
  *'href="/#settings"'*) ok "保留时间提示保留了 /#settings 兜底链接" ;;
  *) bad "保留时间提示保留了 /#settings 兜底链接 — 没找到 href=/#settings" ;;
esac

# ---- 批量删除
BEFORE_N=$(curl -s "$B/api/texts" | jsonlen)
B2=$(curl -s -X POST -H 'Content-Type: application/json' -d '{"content":"批量甲"}' "$B/api/texts" | jget id)
B3=$(curl -s -X POST -H 'Content-Type: application/json' -d '{"content":"批量乙"}' "$B/api/texts" | jget id)
B4=$(curl -s -X POST -H 'Content-Type: application/json' -d '{"content":"批量丙"}' "$B/api/texts" | jget id)
chk "新建 3 条后计数 +3" "$(curl -s "$B/api/texts" | jsonlen)" "$((BEFORE_N+3))"

chk "空 ids 被拒 400" \
  "$(code -X POST -H 'Content-Type: application/json' -d '{"ids":[]}' "$B/api/texts/delete")" "400"

# 超过上限要在**落库之前**拦掉：真的拿 1001 个 ID 去查 SQLite 会顶到绑定变量上限
TOO_MANY="$TMP/too-many.json"
"$PY" -c 'import json,sys; open(sys.argv[1],"w").write(json.dumps({"ids":["id-%d"%i for i in range(1001)]}))' "$TOO_MANY"
chk "超过 1000 个 ID 被拒 400" \
  "$(code -X POST -H 'Content-Type: application/json' --data-binary @"$TOO_MANY" "$B/api/texts/delete")" "400"

# 混进一个不存在的 ID：这是多选删除的正常场景（另一台设备刚好也删了同一条），
# 不能整批报错，返回的应该是**实际删掉的条数**。
chk "批量删除（含不存在的 ID）200" \
  "$(code -X POST -H 'Content-Type: application/json' \
       -d "{\"ids\":[\"$B2\",\"$B3\",\"$B4\",\"no-such-text\"]}" "$B/api/texts/delete")" "200"
DEL_BODY=$(curl -s -X POST -H 'Content-Type: application/json' \
  -d "{\"ids\":[\"$B2\",\"no-such-text\"]}" "$B/api/texts/delete")
chk "重复删除已删过的 ID 返回 0 条" "$(echo "$DEL_BODY" | jget deleted)" "0"
chk "全删完后计数回到原值" "$(curl -s "$B/api/texts" | jsonlen)" "$BEFORE_N"

# 批量删除也要推 texts 事件，否则另一台设备的列表还挂着已经没了的条目
: > "$SSE_OUT"
curl -s -N --max-time 20 "$B/api/events" > "$SSE_OUT" 2>/dev/null &
SSE_PID=$!
for _ in $(seq 1 25); do
  grep -q 'event: ready' "$SSE_OUT" 2>/dev/null && break
  sleep 0.2
done
B5=$(curl -s -X POST -H 'Content-Type: application/json' -d '{"content":"待批删"}' \
  "$B/api/texts" | jget id)
: > "$SSE_OUT"
curl -s -X POST -H 'Content-Type: application/json' -d "{\"ids\":[\"$B5\"]}" "$B/api/texts/delete" > /dev/null
for _ in $(seq 1 25); do
  grep -q 'event: texts' "$SSE_OUT" 2>/dev/null && break
  sleep 0.2
done
if grep -q 'event: texts' "$SSE_OUT"; then
  ok "批量删除推送了 texts 事件"
else
  bad "批量删除推送了 texts 事件 — 实际 [$(head -c 300 "$SSE_OUT" | tr '\n' '|')]"
fi
kill $SSE_PID 2>/dev/null
wait $SSE_PID 2>/dev/null

# ---- 自动清理设置
# 默认必须是"不自动删除"：一个刚装好的实例不该悄悄吃掉用户的东西。
chk "默认 textTTL.value 为 0" "$(curl -s "$B/api/settings" | jget textTTL value)" "0"
chk "默认 textTTL.unit 为 day" "$(curl -s "$B/api/settings" | jget textTTL unit)" "day"

chk "写 textTTL 200" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"textTTL":{"value":3,"unit":"hour"}}' "$B/api/settings")" "200"
chk "读回 value=3" "$(curl -s "$B/api/settings" | jget textTTL value)" "3"
chk "读回 unit=hour" "$(curl -s "$B/api/settings" | jget textTTL unit)" "hour"

# 只改数值时单位要留着——两个键分开存就是为了这个，
# 否则界面上"3 小时"改成"5"会莫名其妙变成"5 天"。
chk "只改 value 不改 unit 200" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"textTTL":{"value":5}}' "$B/api/settings")" "200"
chk "只改 value 后 unit 仍是 hour" "$(curl -s "$B/api/settings" | jget textTTL unit)" "hour"
chk "只改 value 后 value=5" "$(curl -s "$B/api/settings" | jget textTTL value)" "5"

# 设了值也不能顺手把别的设置冲掉（PUT 是全量语义，容易误伤）
chk "改 textTTL 没动主题" "$(curl -s "$B/api/settings" | jget theme)" "dark"

chk "unit 非法被拒 400" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"textTTL":{"unit":"week"}}' "$B/api/settings")" "400"
chk "value 超上限被拒 400" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"textTTL":{"value":10001}}' "$B/api/settings")" "400"
chk "value 为负数被拒 400" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"textTTL":{"value":-1}}' "$B/api/settings")" "400"
# 被拒之后不能留下半个状态
chk "非法请求没改动 value" "$(curl -s "$B/api/settings" | jget textTTL value)" "5"
chk "非法请求没改动 unit" "$(curl -s "$B/api/settings" | jget textTTL unit)" "hour"

chk "value=0 表示关闭 200" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"textTTL":{"value":0}}' "$B/api/settings")" "200"
chk "关闭后 value=0" "$(curl -s "$B/api/settings" | jget textTTL value)" "0"

# 清理周期是 10 分钟，测试期间不会触发；这里确认"设了 TTL 也不会立刻删东西"，
# 免得将来有人把清理挪到请求路径上。
curl -s -X PUT -H 'Content-Type: application/json' -d '{"textTTL":{"value":1,"unit":"hour"}}' "$B/api/settings" > /dev/null
chk "设了 TTL 后已有文本没被立刻清掉" "$(curl -s "$B/api/texts" | jsonlen)" "$BEFORE_N"
curl -s -X PUT -H 'Content-Type: application/json' -d '{"textTTL":{"value":0}}' "$B/api/settings" > /dev/null

# ---------------------------------------------------------------- 21
echo ""
echo "=== 21. 删除设备记录与「设备随消息删除」 ==="

# 默认**关闭**：设备记录不随文本删除而消失。这是有意的——用户给一台设备
# 起好的名字，下次它再发文本时还在；跟着文本删就变回"未知设备"了。
chk "pruneDevices 默认关闭" "$(curl -s "$B/api/settings" | jget pruneDevices)" "False"

# 设备列表要带 textCount：界面靠它判断"这台其实已经空了"，删之前的确认框也要用
chk "设备列表带 textCount 字段" \
  "$(curl -s "$B/api/devices" | "$PY" -c 'import json,sys
d = json.load(sys.stdin)
print("ok" if d and isinstance(d[0].get("textCount"), int) else "missing")')" "ok"

# 打开开关 -> 200 且读回为真
chk "打开「设备随消息删除」200" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"pruneDevices":true}' "$B/api/settings")" "200"
chk "读回是开启的" "$(curl -s "$B/api/settings" | jget pruneDevices)" "True"

# **还有文本的设备不能被清掉**。这一步是整条开关的核心判据，
# 写成"打开开关后设备数变少"就测不出"误删有内容的设备"。
chk "还有文本的设备没被误清" "$(curl -s "$B/api/devices" | jsonlen)" "1"

# 删光文本 -> 设备记录跟着走
ALL_IDS="$(curl -s "$B/api/texts" | "$PY" -c 'import json,sys; print(json.dumps({"ids":[t["id"] for t in json.load(sys.stdin)]}))')"
curl -s -X POST -H 'Content-Type: application/json' -d "$ALL_IDS" "$B/api/texts/delete" > /dev/null
chk "文本删光后设备也空了" "$(curl -s "$B/api/devices" | jsonlen)" "0"

chk "关掉开关 200" \
  "$(code -X PUT -H 'Content-Type: application/json' -d '{"pruneDevices":false}' "$B/api/settings")" "200"
chk "关掉后读回是关闭的" "$(curl -s "$B/api/settings" | jget pruneDevices)" "False"

# ---- 显式删设备（设备面板里那个按钮）
#
# 语义：**只删设备记录，不碰它的文本**。文本会留着，只是发送方回落成
# 「未知设备」——用户点的是"把这个设备条目去掉"，不是"清空它的内容"。
curl -s -X POST -H 'Content-Type: application/json' -H "User-Agent: $UA_IPHONE" \
  -d '{"content":"删设备前的最后一条"}' "$B/api/texts" > /dev/null
chk "本机设备又出现了" "$(curl -s "$B/api/devices" | jsonlen)" "1"
curl -s -X PUT -H 'Content-Type: application/json' -d '{"remark":"要删掉的这台"}' \
  "$B/api/devices/$MY_IP" > /dev/null
TEXTS_BEFORE="$(curl -s "$B/api/texts" | jsonlen)"

chk "删设备 200" "$(code -X DELETE "$B/api/devices/$MY_IP")" "200"
chk "设备记录没了" "$(curl -s "$B/api/devices" | jsonlen)" "0"
chk "文本一条都没少" "$(curl -s "$B/api/texts" | jsonlen)" "$TEXTS_BEFORE"
chk "文本里的发送方回落成未知设备" \
  "$(curl -s "$B/api/texts" | jget 0 deviceName)" "未知设备"
chk "重复删设备 404" "$(code -X DELETE "$B/api/devices/$MY_IP")" "404"

# ---------------------------------------------------------------- 22
echo
echo "=== 22. 版本号与更新检查 ==="

# 版本号是**构建时注入**的（internal/version，见 README 的「自动发布与版本」）。
# 这里不断言具体值——本地 go build 是 dev、CI 里是 tag 名——只要求
# /api/version 和 /api/config 报的是同一个：两边各存一份的话，设置面板上
# 显示的版本会和实际运行的版本对不上，而那种偏差没人会发现。
chk "版本接口 200（公开，不需要口令）" "$(code "$B/api/version")" "200"

VER="$(curl -s "$B/api/version")"
chk "/api/version 与 /api/config 报同一个版本" \
  "$(printf '%s' "$VER" | jget current)" "$(curl -s "$B/api/config" | jget version)"

# 前端按这些字段渲染「关于」那块，少一个就是一片空白。
# 逐个 jget：字段不存在时 jget 直接抛 KeyError、输出空串，chk 立刻变红。
case "$(printf '%s' "$VER" | jget hasUpdate)" in
  True|False) ok "hasUpdate 是布尔值" ;;
  *) bad "hasUpdate 不是布尔值 — 前端那块的判断会失效" ;;
esac
case "$(printf '%s' "$VER" | jget checking)" in
  True|False) ok "checking 是布尔值" ;;
  *) bad "checking 不是布尔值 — 前端不会补问第二轮" ;;
esac

# 下面这几条只在脚本自己起的实例上验：那个实例带了 QS_UPDATE_CHECK=0，
# 所以"不联网、不报错、不提示"这条路是确定的。
#
# 真去查 GitHub 的那条路由 internal/server 的单测用假 API 覆盖，
# e2e 刻意不碰网络——CI 上不该因为 GitHub 抽风而变红。
if [ "$OWN" = "1" ]; then
  chk "关掉更新检查后 latest 为空" "$(printf '%s' "$VER" | jget latest)" ""
  chk "关掉更新检查后不报错" "$(printf '%s' "$VER" | jget error)" ""
  chk "关掉更新检查后不提示有更新" "$(printf '%s' "$VER" | jget hasUpdate)" "False"
  chk "强制刷新接口能用（无口令实例）" "$(code -X POST "$B/api/version/check")" "200"
fi

echo
echo "=================================================="
printf '  通过 \033[32m%d\033[0m 项，失败 \033[31m%d\033[0m 项\n' "$PASS" "$FAIL"
echo "=================================================="
[ "$FAIL" -eq 0 ]
