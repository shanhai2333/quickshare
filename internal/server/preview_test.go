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
