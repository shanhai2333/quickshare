package server

import "testing"

// previewKindOf 是前端「预览」按钮的准入判断：只有它返回非空，前端才会给出按钮。
//
// 这张表**故意把白名单里的每个类型都列一遍**。往 inlineSafe / sandboxInline 里加了
// 新类型却忘了想清楚"要不要预览"时，这个测试会红——那正是要提醒的时刻：
// 是把它归到某一类，还是明确地不给预览。两件事都必须是有意为之。
func TestPreviewKindOf(t *testing.T) {
	cases := []struct {
		mt   string
		want string
	}{
		// 图片
		{"image/jpeg", "image"},
		{"image/png", "image"},
		{"image/gif", "image"},
		{"image/webp", "image"},
		{"image/avif", "image"},
		{"image/bmp", "image"},
		{"image/tiff", "image"},
		{"image/x-icon", "image"},
		{"image/vnd.microsoft.icon", "image"},
		// SVG 归到 image：它内联时带 CSP sandbox，放在 <img> 里脚本本来也跑不起来
		{"image/svg+xml", "image"},

		// 视频
		{"video/mp4", "video"},
		{"video/webm", "video"},
		{"video/ogg", "video"},
		{"video/quicktime", "video"},

		// 音频
		{"audio/mpeg", "audio"},
		{"audio/ogg", "audio"},
		{"audio/wav", "audio"},
		{"audio/x-wav", "audio"},
		{"audio/webm", "audio"},
		{"audio/mp4", "audio"},
		{"audio/flac", "audio"},

		// 文档与文本
		{"application/pdf", "pdf"},
		{"text/plain", "text"},
		{"text/csv", "text"},
		{"text/markdown", "text"},
		{"application/json", "text"},

		// 带参数的要能解析出来
		{"text/plain; charset=utf-8", "text"},
		{"IMAGE/PNG", "image"},
		{"  image/jpeg  ", "image"},

		// ---- 下面这些**必须**没有预览
		//
		// text/html 是重点：它能上传、能在本站源里执行脚本（存储型 XSS 那条），
		// 所以既不在 inlineSafe 里，也不能给预览按钮。谁要是把它加进白名单，
		// 上面那张表不会红，但这一条会。
		{"text/html", ""},
		{"application/xhtml+xml", ""},
		{"application/octet-stream", ""},
		{"application/zip", ""},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", ""},
		{"application/vnd.openxmlformats-officedocument.presentationml.presentation", ""},
		{"application/msword", ""},
		{"text/javascript", ""},
		{"", ""},
		{"image", ""},
	}

	for _, c := range cases {
		if got := previewKindOf(c.mt); got != c.want {
			t.Errorf("previewKindOf(%q) = %q, 期望 %q", c.mt, got, c.want)
		}
	}
}

// 核心不变量：**给了预览的类型，必须真的能内联渲染。**
//
// 前端拿到非空 preview 就显示「预览」按钮，用户点下去期望看到内容；如果那个类型
// 实际是 modeDownload，点一下会变成下载文件——功能静默失效，而且是在真实浏览器里
// 才看得出来。这条断言把两个函数钉在一起：以后谁把 previewKindOf 改成另写一份
// 类型清单，只要两份不一致就会红。
func TestPreviewKindImpliesInline(t *testing.T) {
	all := make([]string, 0, len(inlineSafe)+len(sandboxInline))
	for mt := range inlineSafe {
		all = append(all, mt)
	}
	for mt := range sandboxInline {
		all = append(all, mt)
	}

	for _, mt := range all {
		if previewKindOf(mt) == "" {
			t.Errorf("%q 在白名单里能内联，却没有预览归类——加了类型就得想清楚要不要预览", mt)
		}
		if previewModeOf(mt) == modeDownload {
			t.Errorf("%q 有预览归类，但实际会被强制下载", mt)
		}
	}
}

// 反过来：不能预览的类型，必须是"真的不能内联"，而不是"归类漏了"。
func TestNoPreviewMeansDownload(t *testing.T) {
	for _, mt := range []string{"text/html", "application/zip", "application/octet-stream"} {
		if previewKindOf(mt) != "" {
			t.Errorf("%q 不该有预览", mt)
		}
		if previewModeOf(mt) != modeDownload {
			t.Errorf("%q 不该被内联", mt)
		}
	}
}

// 文本类文件在**上传时**被规范成 text/plain（`normalizeUploadMime`，由
// `handleUploadInit` 调用）。这个函数的安全性质只有一条：**它只会把类型收窄成
// text/plain，永远不会放宽成别的**——所以下面逐条钉住"表里每个扩展名都落到
// text/plain，而且落点必须真的可内联"，以及"不在表里的一律原样返回"。
//
// 注意它**不改变下载准入**：`text/html` 作为**类型**依然不能内联（见
// TestNoPreviewMeansDownload）。变的是 `.html` **文件**上传时被存成 text/plain。
func TestNormalizeUploadMime(t *testing.T) {
	// 1) 表里每一个都必须规范成 text/plain，且落点可内联、归到 text 类
	for ext := range textExts {
		got := normalizeUploadMime("x"+ext, "application/octet-stream")
		if got != "text/plain" {
			t.Errorf("%s 该被规范成 text/plain，实际 %q", ext, got)
		}
		if previewModeOf(got) != modeInline {
			t.Errorf("%s 规范后的类型不能内联，那规范化就没意义", ext)
		}
		if previewKindOf(got) != "text" {
			t.Errorf("%s 规范后该归到 text，实际 %q", ext, previewKindOf(got))
		}
	}

	// 2) 没有扩展名的文本文件名（filepath.Ext 拿不到东西），大小写不敏感
	for _, name := range []string{
		"Makefile", "MAKEFILE", "makefile", "GNUmakefile", "Dockerfile", "Containerfile",
		"LICENSE", "License", "README", "CHANGELOG", "Procfile", "Gemfile",
		// 点开头的：filepath.Ext 会把整个名字当扩展名返回
		".gitignore", ".env", ".editorconfig",
	} {
		if got := normalizeUploadMime(name, "application/octet-stream"); got != "text/plain" {
			t.Errorf("%s 该被规范成 text/plain，实际 %q", name, got)
		}
	}

	// 3) **默认拒绝**：不在表里的一律原样返回，一个字节都不许改
	for _, name := range []string{
		"x.zip", "x.exe", "x.png", "x.jpg", "x.pdf", "x.mp4", "x.mp3", "x.docx", "x.rar",
		"x.unknown", "noext", "x.", "", "x.tar.gz",
		// 这几个的真实类型本来就在白名单里，改写只会引入无谓差异
		"x.txt", "x.csv", "x.md", "x.json",
		// SVG 必须保持 image/svg+xml：它是 sandboxInline，当图片看比看源码有用
		"x.svg",
	} {
		if got := normalizeUploadMime(name, "application/zip"); got != "application/zip" {
			t.Errorf("%s 不该被改写，实际 %q", name, got)
		}
	}
	if got := normalizeUploadMime("x.svg", "image/svg+xml"); got != "image/svg+xml" {
		t.Fatalf("SVG 必须保持 image/svg+xml，实际 %q", got)
	}
}

// 反过来钉一条：**规范化绝不能把某个类型变成"能执行脚本"的类型**。
//
// 现在只有 text/plain 一个落点，所以这条是恒真的；但它是"这张表以后怎么加都安全"的
// 依据——哪天有人让 normalizeUploadMime 也返回 text/html 之类，这里会红。
func TestNormalizeUploadMimeNeverYieldsScriptable(t *testing.T) {
	scriptable := map[string]bool{
		"text/html": true, "application/xhtml+xml": true,
		"text/xml": true, "application/xml": true,
		"text/javascript": true, "application/javascript": true,
		"application/x-shockwave-flash": true,
	}
	for ext := range textExts {
		got := normalizeUploadMime("x"+ext, "application/octet-stream")
		if scriptable[normalizedMIME(got)] {
			t.Errorf("%s 被规范成了可执行脚本的类型 %q——这正是那条存储型 XSS", ext, got)
		}
	}
}

// 歧义扩展名：**同一后缀既有常见的文本形态、又有常见的二进制形态**，一律不进 textExts。
//
// 2026-09-19 实测踩过：`.ts` 被当成 TypeScript 收进了表里，结果下载的 HLS 分片 /
// 录制的电视节目（MPEG 传输流，也是 `.ts`）全被规范成 text/plain —— 点开是乱码，
// 链接也不再是下载。这条测试就是防止有人"顺手补上"。
func TestAmbiguousExtensionsStayOutOfTextExts(t *testing.T) {
	cases := []struct{ ext, why string }{
		{".ts", "MPEG 传输流（HLS 分片 / 录制电视）比 TypeScript 常见得多"},
		{".sub", "VobSub 的 .sub 是二进制，MicroDVD 的才是文本"},
		{".mod", "Amiga 音乐模块 / Fortran 模块都是二进制，只有 go.mod 是文本"},
		{".bin", "二进制"},
		{".dat", "二进制"},
		{".img", "二进制"},
		{".iso", "二进制"},
		{".dump", "多为二进制内存转储"},
		{".bak", "备份的原件可能是任何东西"},
	}
	for _, c := range cases {
		if textExts[c.ext] {
			t.Errorf("%s 不该进 textExts（%s）", c.ext, c.why)
		}
	}
	// 反例：这些后缀没有二进制歧义，应该在表里
	for _, ext := range []string{".py", ".go", ".log", ".yml", ".html", ".tsx"} {
		if !textExts[ext] {
			t.Errorf("%s 是纯文本后缀，该在 textExts 里", ext)
		}
	}
}

// SVG 是唯一"既是图片、又必须沙箱"的类型：归到 image 类，但内联时一定带 CSP sandbox。
// 这两件事拆开看都无害，合起来才是安全的，所以钉一条。
func TestSVGIsImageButSandboxed(t *testing.T) {
	if got := previewKindOf("image/svg+xml"); got != "image" {
		t.Fatalf("SVG 该归到 image，实际 %q", got)
	}
	if got := previewModeOf("image/svg+xml"); got != modeSandboxed {
		t.Fatalf("SVG 必须走 sandbox，实际 %v", got)
	}
}
