#!/usr/bin/env python3
"""打包源码快照 go.zip（分享给别人看 / 编译用）。纯标准库，无依赖。

    python pack.py

为什么要有这个脚本，而不是手写一条 zip 命令：
**手写的文件列表一定会漏。** 之前那份快照就是手写的，漏掉了全部文本功能相关文件
（`internal/server/texts.go`、`internal/store/texts.go`、两个对应的 `_test.go`、
`web/text.html`、`web/text.js`、`web/live.js`）——而 `server.go` 引用了 `texts.go` 里的东西，
**别人拿到那份快照根本编译不过**；更糟的是漏的都是同一块功能，看着不像"漏了"，
倒像"这个项目就没有文本功能"。

所以这里改成**遍历项目目录**：新增文件会自动进快照，不需要谁记得回来改脚本。
排除项和 `.gitignore` 保持一致（运行数据、编译产物、临时目录）。

打完之后还会**把快照解到临时目录真编译一遍**——这是唯一能证明"没漏文件"的办法。
清单对不对、目录全不全，都不如让编译器说一句话。
"""

import subprocess
import sys
import tempfile
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent
OUT = ROOT / "go.zip"

# 不进快照的东西。改这里之前先想清楚：这些是"运行时会重新生成"或"本机私有"的，
# 不是源码的一部分。
EXCLUDE_DIRS = {
    ".git",          # 版本库
    ".tmp",          # 本地临时/测试目录
    ".workbuddy-ai", # 项目记忆与技能数据，属于工作台，不属于源码
    "data",          # 运行数据：文件本体、分片、SQLite、背景图
    "dist",          # 编译产物
}
EXCLUDE_FILES = {
    "go.zip",        # 快照自己，别套娃
    ".DS_Store",
}

# 兜底：这些后缀即使在上面没被排除，也不该进快照。
# `.zip` 也在内——项目根上随手打出来的压缩包（比如某次留下的 internal.zip）
# 不是源码，卷进快照只会让人分不清哪个才是真的。
EXCLUDE_SUFFIXES = (".db", ".db-wal", ".db-shm", ".exe", ".pyc", ".zip")


def collect():
    files = []
    for p in sorted(ROOT.rglob("*")):
        if not p.is_file():
            continue
        rel = p.relative_to(ROOT)
        if any(part in EXCLUDE_DIRS for part in rel.parts[:-1]):
            continue
        if rel.name in EXCLUDE_FILES:
            continue
        if rel.name.endswith(EXCLUDE_SUFFIXES):
            continue
        files.append((p, rel))
    return files


def verify():
    """解到临时目录编译一遍。返回 (ok, 输出)。"""
    try:
        with tempfile.TemporaryDirectory(prefix="qspack-") as tmp:
            with zipfile.ZipFile(OUT) as z:
                z.extractall(tmp)
            r = subprocess.run(["go", "vet", "./..."], cwd=tmp,
                               capture_output=True, text=True)
            return r.returncode == 0, (r.stdout + r.stderr).strip()
    except FileNotFoundError:
        return None, "本机没有 go 命令，跳过编译验证"


def main():
    files = collect()
    if not files:
        print("没找到任何文件，是不是跑错目录了？")
        return 1

    with zipfile.ZipFile(OUT, "w", zipfile.ZIP_DEFLATED) as z:
        for path, rel in files:
            z.write(path, rel.as_posix())

    print(f"已生成 {OUT.name}：{len(files)} 个文件，{OUT.stat().st_size / 1024:.1f} KB")
    # 按顶层目录分组打印，方便一眼看出有没有整块缺失
    groups = {}
    for _, rel in files:
        top = rel.parts[0] if len(rel.parts) > 1 else "(根目录)"
        groups.setdefault(top, []).append(rel.as_posix())
    print()
    for top in sorted(groups):
        print(f"  {top}/  ({len(groups[top])} 个)")

    print("\n正在把快照解到临时目录编译验证…")
    ok, out = verify()
    if ok is None:
        print(f"  {out}")
        return 0
    if not ok:
        print("  编译不过——快照八成漏了文件，下面是编译器的原话：\n")
        print("  " + "\n  ".join(out.splitlines()[:20]))
        return 1
    print("  编译通过 ✓ 快照是完整的")
    return 0


if __name__ == "__main__":
    sys.exit(main())
