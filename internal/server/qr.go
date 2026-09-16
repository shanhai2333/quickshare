package server

import (
	"log"
	"net/http"

	"github.com/skip2/go-qrcode"
)

// 二维码的几个固定参数。
const (
	// 每个模块渲染成多少像素。太小会糊到扫不出来。
	qrScale = 6

	// 图的最小边长。小内容（版本 1）只有 21 个模块，按比例算出来才一百多像素，
	// 前端一放大就糊，所以兜一个下限。
	qrMinSize = 256

	// 内容字节数上限。二维码本身最多能装 2953 字节（版本 40 / 纠错 L），
	// 但这里只服务于"把 URL 变成码"这一件事，正常不到 200 字节。
	// 卡在这里是为了挡住"拿它当通用二维码生成器"的用法——那是白嫖 CPU。
	qrMaxLen = 1024
)

// handleQR 把一段文本渲染成二维码 PNG。
//
// 内容由调用方通过 ?d= 传入，服务端不读自己的任何数据，所以不鉴权。
// 代价是它等价于一个"任意文本转二维码"接口；内网自用场景可以接受，
// qrMaxLen 就是为这个留的一道闸。
func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("d")
	if text == "" {
		http.Error(w, "缺少参数 d", http.StatusBadRequest)
		return
	}
	if len(text) > qrMaxLen {
		http.Error(w, "内容过长（上限 1024 字节）", http.StatusBadRequest)
		return
	}

	qr, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		// 内容超出二维码容量（版本 40 能装 2953 字节）时会走到这里
		http.Error(w, "内容无法编码成二维码", http.StatusBadRequest)
		return
	}

	// 边长跟着模块数走，而不是固定值。
	//
	// 这是个容易踩的坑：内容越长二维码版本越高、模块越多（版本 1 是 21×21，
	// 版本 40 是 177×177）。写死 256 的话，长 URL 会把每个模块挤到不足一个像素——
	// 图看着还是那个图，就是怎么扫都扫不出来。文件名里有中文时 URL 会按
	// %XX 膨胀成三倍长，很容易撞上。
	size := (len(qr.Bitmap()) + 8) * qrScale // +8 是规范要求的静默区（四边各 4 模块）
	if size < qrMinSize {
		size = qrMinSize
	}

	png, err := qr.PNG(size)
	if err != nil {
		http.Error(w, "二维码生成失败", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	// 内容完全由 URL 决定，同一个 URL 永远得到同一张图，可以放心长缓存
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if _, err := w.Write(png); err != nil {
		log.Printf("二维码写出失败: %v", err)
	}
}
