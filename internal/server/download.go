package server

import (
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
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
//
// **文本类文件（.html / .py / .log …）不在这里加它们的真实类型**——它们靠
// textExts 在上传时就被规范成 text/plain（见下面那段注释），所以能看源码、
// 但绝不会被当 HTML/XML 渲染。
var inlineSafe = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/gif": true,
	"image/webp": true, "image/avif": true, "image/bmp": true,
	"image/tiff": true, "image/x-icon": true, "image/vnd.microsoft.icon": true,
	"video/mp4": true, "video/webm": true, "video/ogg": true, "video/quicktime": true,
	"audio/mpeg": true, "audio/ogg": true, "audio/wav": true, "audio/x-wav": true,
	"audio/webm": true, "audio/mp4": true, "audio/x-m4a": true, "audio/flac": true,
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

// ---------------------------------------------------------------- 文本类文件的规范化

// textExts / textNames 是"内容一定是纯文本"的扩展名与文件名。命中的文件在**上传时**
// 就把 MIME 规范化成 text/plain（`normalizeUploadMime`，由 `handleUploadInit` 调用）。
//
// 为什么不是把这些扩展名的**真实类型**加进 inlineSafe：
//
//   - `.html` 的真实类型是 `text/html`，内联 = 存储型 XSS（上传的 HTML 在**本站源**里
//     渲染，脚本能读到 localStorage 里的管理口令）
//   - `.xml` 的 `text/xml` 内联时，`<?xml-stylesheet?>` 指到的 XSLT 在 Chrome 里**会执行**，
//     输出 HTML 照样能跑脚本
//   - `.js` / `.css` 同理
//
// 规范化成 text/plain 之后这些文件**能看源码、但不会被渲染**，而且 `text/plain` 本来
// 就在 inlineSafe 里。安全性靠两件事兜住：① text/plain 不是脚本上下文；② 全局的
// `X-Content-Type-Options: nosniff`（`securityHeaders` 设的，`/f/` 也走）让浏览器
// 不会拿真实内容去嗅探类型。用户要原件就点「下载」——那个链接带 `?dl=1`，强制 attachment。
//
// 边界（都别顺手加进来）：
//   - **`.svg` 不在表里**：它是 `image/svg+xml`（sandboxInline），当图片看比看源码有用
//   - **`.txt` / `.csv` / `.md` / `.json` 不在表里**：真实类型本来就在白名单里，没必要改写
//   - `.zip` / `.docx` / `.png` 这些非文本不在表里——**默认拒绝**，宁可只给下载
//
// 这张表只影响"上传时存成什么类型"，**不参与下载准入**：准入始终只有 previewModeOf 一处。
var textExts = map[string]bool{
	// 源码与脚本
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".cxx": true, ".hpp": true,
	".cs": true, ".java": true, ".kt": true, ".go": true, ".rs": true,
	".py": true, ".pyw": true, ".rb": true, ".php": true, ".pl": true, ".lua": true,
	".sh": true, ".bash": true, ".zsh": true, ".fish": true, ".bat": true, ".cmd": true,
	".ps1": true, ".psm1": true, ".vbs": true, ".awk": true, ".sed": true, ".tcl": true,
	".swift": true, ".scala": true, ".dart": true, ".m": true, ".r": true, ".jl": true,
	".sql": true, ".graphql": true, ".proto": true, ".asm": true, ".s": true,
	// 前端（注意 **`.ts` 不在里面**：它更常见的身份是 MPEG 传输流——下载的 HLS 分片、
	// 录制的电视节目都是 `.ts`，当 TypeScript 规范成纯文本会让那些视频变成一堆乱码。
	// `.tsx` 没有这层歧义，所以它在。这条歧义单测钉着，别"顺手补上"）
	".js": true, ".mjs": true, ".cjs": true, ".jsx": true, ".tsx": true,
	".vue": true, ".svelte": true,
	".css": true, ".scss": true, ".sass": true, ".less": true, ".styl": true,
	".html": true, ".htm": true, ".xhtml": true,
	// 标记与文档
	".xml": true, ".xsl": true, ".xslt": true, ".dtd": true, ".plist": true,
	".rst": true, ".adoc": true, ".org": true, ".tex": true, ".bib": true, ".wiki": true,
	// 配置与构建
	".yml": true, ".yaml": true, ".toml": true, ".ini": true, ".conf": true, ".cfg": true,
	".cnf": true, ".properties": true, ".env": true, ".rc": true,
	".mk": true, ".cmake": true, ".sum": true, ".lock": true, ".spec": true,
	".gradle": true, ".hcl": true, ".tf": true, ".tfvars": true, ".nix": true,
	".editorconfig": true, ".gitignore": true, ".gitattributes": true,
	".dockerignore": true, ".npmrc": true, ".yarnrc": true, ".prettierrc": true,
	".eslintrc": true, ".babelrc": true, ".htaccess": true,
	".desktop": true, ".service": true, ".rules": true,
	// 日志与输出
	".log": true, ".out": true, ".err": true, ".trace": true,
	// 字幕与播放列表
	".srt": true, ".ass": true, ".vtt": true, ".lrc": true, ".cue": true,
	".m3u": true, ".m3u8": true,
	// 补丁、差异与其余文本
	".diff": true, ".patch": true, ".nzb": true, ".json5": true, ".jsonc": true,
	".tsv": true, ".nfo": true, ".url": true, ".ipynb": true, ".har": true,
}

// textNames 是没有扩展名、但确定是纯文本的文件名（`filepath.Ext` 拿不到东西）。
// 键一律小写，比对时把名字转小写。
var textNames = map[string]bool{
	"makefile": true, "gnumakefile": true, "dockerfile": true, "containerfile": true,
	"justfile": true, "procfile": true, "gemfile": true, "rakefile": true,
	"vagrantfile": true, "brewfile": true,
	"license": true, "licence": true, "copying": true, "notice": true,
	"readme": true, "changelog": true, "changes": true, "authors": true,
	"contributors": true, "version": true, "todo": true,
}

// normalizeUploadMime 把"确定是纯文本"的文件在上传时就规范成 text/plain。
//
// 不是纯文本就**原样返回**——规范化只会收窄类型，不会放宽（这是它的安全性质，
// 单测 `TestNormalizeUploadMime` 钉着）。
func normalizeUploadMime(name, mime string) string {
	if isPlainTextName(name) {
		return "text/plain"
	}
	return mime
}

// isPlainTextName 按文件名判断是不是"确定是纯文本"。
func isPlainTextName(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	if textNames[base] {
		return true
	}
	// 注意 `.gitignore` 这类点开头文件：filepath.Ext 会把整个名字当扩展名返回，
	// 所以它们在 textExts 里也是按 ".gitignore" 这个键匹配的。
	return textExts[filepath.Ext(base)]
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
