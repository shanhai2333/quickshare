package server

import (
	"fmt"
	"mime"
	"net/http"
	"os"
	"strings"
)

// previewMode 描述一个 MIME 类型该怎么回吐给浏览器。
type previewMode int

const (
	modeDownload  previewMode = iota // 强制下载
	modeInline                       // 直接内联预览
	modeSandboxed                    // 内联预览，但加 CSP sandbox 掐掉脚本能力
)

// inlineSafe 是可以放心内联预览的类型。
//
// 这里用白名单，而不是早先那种 "text/" "image/" 前缀匹配——前缀匹配会把
// text/html 一起放进来，而上传的 HTML 是能在**本站源**上执行脚本的：
// 传一个 .html 再点开，脚本就能读到 localStorage 里的管理口令，
// 然后以管理员身份调所有接口。默认拒绝，新类型要显式加进来。
var inlineSafe = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/gif": true,
	"image/webp": true, "image/avif": true, "image/bmp": true,
	"image/tiff": true, "image/x-icon": true, "image/vnd.microsoft.icon": true,
	"video/mp4": true, "video/webm": true, "video/ogg": true, "video/quicktime": true,
	"audio/mpeg": true, "audio/ogg": true, "audio/wav": true, "audio/x-wav": true,
	"audio/webm": true, "audio/mp4": true, "audio/flac": true,
	"application/pdf": true,
	"text/plain":      true, "text/csv": true, "text/markdown": true,
	"application/json": true,
}

// sandboxInline 是想保留预览、但必须靠 CSP sandbox 掐掉脚本能力的类型。
//
// SVG 是图片，一律强制下载体验太差；但它能内嵌 <script>，所以让它在
// 不透明源里渲染：图照常看得见，脚本跑不起来，也读不到 localStorage。
var sandboxInline = map[string]bool{
	"image/svg+xml": true,
}

// normalizedMIME 去掉 charset 之类的参数、转小写，得到能直接比对的媒体类型。
func normalizedMIME(contentType string) string {
	mt := contentType
	if parsed, _, err := mime.ParseMediaType(contentType); err == nil {
		mt = parsed
	}
	return strings.ToLower(strings.TrimSpace(mt))
}

// previewModeOf 判断该类型用什么方式回吐。
func previewModeOf(contentType string) previewMode {
	switch mt := normalizedMIME(contentType); {
	case inlineSafe[mt]:
		return modeInline
	case sandboxInline[mt]:
		return modeSandboxed
	default:
		return modeDownload
	}
}

// previewKindOf 把类型归到前端预览层能用的那一类，归不进去返回空串。
//
// **它先问 previewModeOf，而不是另写一份类型清单。** 前端只在拿到非空值时才显示
// 「预览」按钮，而那个按钮点下去必须真的能内联渲染——两份清单一旦漂了，表现是
// "点预览直接触发下载"（或者更糟：把不该内联的类型放进预览）。
// 所以这里只做"把白名单里的类型再分个组"，准入判断始终只有 previewModeOf 一处。
//
// 注意 `text/*` 这个前缀在这里是安全的：能走到这一步说明它已经在 inlineSafe 里了，
// 而 `text/html` 不在（理由见上面 inlineSafe 的注释）。
func previewKindOf(contentType string) string {
	if previewModeOf(contentType) == modeDownload {
		return ""
	}
	switch mt := normalizedMIME(contentType); {
	case strings.HasPrefix(mt, "image/"):
		return "image"
	case strings.HasPrefix(mt, "video/"):
		return "video"
	case strings.HasPrefix(mt, "audio/"):
		return "audio"
	case mt == "application/pdf":
		return "pdf"
	case mt == "application/json", strings.HasPrefix(mt, "text/"):
		return "text"
	}
	return ""
}

// handleDownload 按文件 ID 下载。
//
// 下载走 http.ServeContent，因此天然支持：
//   - HTTP Range 断点续传
//   - 视频/音频边下边播
//   - If-Modified-Since 条件请求
//
// URL 末尾的文件名只是为了让链接看起来友好，不参与定位，实际以 ID 为准。
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	b := s.be()
	id := r.PathValue("id")

	f, err := b.st.GetFile(id)
	if err != nil || f.Status != "ready" {
		writeErr(w, http.StatusNotFound, "文件不存在")
		return
	}

	file, err := os.Open(b.filePath(id))
	if err != nil {
		writeErr(w, http.StatusNotFound, "文件已从磁盘移除")
		return
	}
	defer func() { _ = file.Close() }()

	stat, err := file.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取文件信息失败")
		return
	}

	contentType := f.Mime
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	mode := previewModeOf(contentType)
	if r.URL.Query().Get("dl") == "1" {
		mode = modeDownload
	}

	disposition := "attachment"
	switch mode {
	case modeInline:
		disposition = "inline"
	case modeSandboxed:
		disposition = "inline"
		// 不透明源 + 禁止一切子资源：图能看，脚本动不了
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", contentDisposition(disposition, f.Name))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, max-age=0")

	http.ServeContent(w, r, f.Name, stat.ModTime(), file)
}

// contentDisposition 生成符合 RFC 6266 的头部，兼容中文文件名。
func contentDisposition(kind, filename string) string {
	ascii := make([]rune, 0, len(filename))
	for _, r := range filename {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			ascii = append(ascii, '_')
			continue
		}
		ascii = append(ascii, r)
	}
	fallback := strings.TrimSpace(string(ascii))
	if fallback == "" {
		fallback = "download"
	}
	return fmt.Sprintf("%s; filename=\"%s\"; filename*=UTF-8''%s",
		kind, fallback, percentEncode(filename))
}

// percentEncode 按 RFC 5987 转义文件名。
func percentEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteString(hexByte(c))
		}
	}
	return b.String()
}
