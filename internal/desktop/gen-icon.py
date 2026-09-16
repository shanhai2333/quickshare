"""生成托盘图标 icon.png（纯标准库，无第三方依赖）。

图案与网页 favicon 一致：蓝色圆角方块 + 白色下载箭头。
用 8 倍超采样做抗锯齿，最后降采样回 32x32。

**平时不需要跑这个脚本**——icon.png 已经提交在仓库里，go:embed 直接用它。
只有想换图标外观时才需要重新生成：

    python3 internal/desktop/gen-icon.py internal/desktop/icon.png

生成后跑一遍 `go test ./internal/desktop/`：里面的 TestIconFromPNG 会确认
新图确实是 Windows 认得的图标资源（认不得的话托盘会悄悄退回系统默认图标）。
"""

import math
import struct
import zlib

SIZE = 32
SS = 8          # 超采样倍率
RADIUS = 8.0    # 圆角半径
BLUE = (47, 111, 237)
WHITE = (255, 255, 255)
STROKE = 2.2

# 与 favicon 的 path 对齐：M16 8v12m0 0l-5-5m5 5l5-5M9 23h14
SEGMENTS = [
    ((16.0, 8.0), (16.0, 20.0)),
    ((16.0, 20.0), (11.0, 15.0)),
    ((16.0, 20.0), (21.0, 15.0)),
    ((9.0, 23.0), (23.0, 23.0)),
]


def in_rounded_rect(x, y, w, h, r):
    if x < 0 or y < 0 or x > w or y > h:
        return False
    cx = min(max(x, r), w - r)
    cy = min(max(y, r), h - r)
    if x == cx or y == cy:
        return True
    return (x - cx) ** 2 + (y - cy) ** 2 <= r * r


def dist_to_segment(px, py, a, b):
    ax, ay = a
    bx, by = b
    dx, dy = bx - ax, by - ay
    denom = dx * dx + dy * dy
    t = 0.0 if denom == 0 else ((px - ax) * dx + (py - ay) * dy) / denom
    t = max(0.0, min(1.0, t))
    qx, qy = ax + t * dx, ay + t * dy
    return math.hypot(px - qx, py - qy)


def sample(x, y):
    """返回该点的 (r, g, b, a)。"""
    if not in_rounded_rect(x, y, SIZE, SIZE, RADIUS):
        return (0, 0, 0, 0)
    d = min(dist_to_segment(x, y, a, b) for a, b in SEGMENTS)
    if d <= STROKE / 2:
        return WHITE + (255,)
    return BLUE + (255,)


def render():
    rows = []
    half = 0.5 / SS
    for py in range(SIZE):
        row = bytearray()
        for px in range(SIZE):
            acc = [0.0, 0.0, 0.0, 0.0]
            for sy in range(SS):
                for sx in range(SS):
                    x = px + (sx + 0.5) / SS
                    y = py + (sy + 0.5) / SS
                    r, g, b, a = sample(x, y)
                    # 预乘 alpha 再平均，避免透明边缘混进黑边
                    acc[0] += r * a / 255.0
                    acc[1] += g * a / 255.0
                    acc[2] += b * a / 255.0
                    acc[3] += a
            n = SS * SS
            a = acc[3] / n
            if a <= 0:
                row += bytes((0, 0, 0, 0))
            else:
                # acc[i] 存的是 r*(a/255) 的累加，除以 n 得到预乘均值，
                # 再除以 alpha 比例还原成直通色
                def unpremul(v):
                    return max(0, min(255, round(v / n * 255.0 / a)))
                row += bytes((
                    unpremul(acc[0]),
                    unpremul(acc[1]),
                    unpremul(acc[2]),
                    max(0, min(255, round(a))),
                ))
        rows.append(bytes(row))
    return rows


def png_bytes(rows):
    raw = b"".join(b"\x00" + r for r in rows)

    def chunk(tag, data):
        return (struct.pack(">I", len(data)) + tag + data
                + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF))

    ihdr = struct.pack(">IIBBBBB", SIZE, SIZE, 8, 6, 0, 0, 0)
    return (b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", ihdr)
            + chunk(b"IDAT", zlib.compress(raw, 9))
            + chunk(b"IEND", b""))


if __name__ == "__main__":
    import sys
    out = sys.argv[1] if len(sys.argv) > 1 else "internal/tray/icon.png"
    data = png_bytes(render())
    with open(out, "wb") as f:
        f.write(data)
    print(f"{out}: {len(data)} bytes")
