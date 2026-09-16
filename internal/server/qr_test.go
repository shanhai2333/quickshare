package server

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

var pngMagic = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// handleQR 不读 Server 的任何字段，空构造就够了
func qrGet(d string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/qr?d="+url.QueryEscape(d), nil)
	rec := httptest.NewRecorder()
	(&Server{}).handleQR(rec, req)
	return rec
}

func TestQRReturnsPNG(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"普通下载链接", "http://192.168.1.100:8080/f/abc123/report.pdf"},
		{"中文文件名（URL 会按 %XX 膨胀三倍）", "http://192.168.1.100:8080/f/abc123/%E5%B9%B4%E5%BA%A6%E6%8A%A5%E5%91%8A.pdf"},
		{"首页地址", "http://192.168.1.100:8080/"},
	}
	for _, c := range cases {
		rec := qrGet(c.text)
		if rec.Code != http.StatusOK {
			t.Errorf("%s：状态码 = %d，期望 200（%s）", c.name, rec.Code, rec.Body.String())
			continue
		}
		body := rec.Body.Bytes()
		if !bytes.HasPrefix(body, pngMagic) {
			t.Errorf("%s：返回的不是 PNG", c.name)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("%s：Content-Type = %q，期望 image/png", c.name, ct)
		}
		// 图里必须真的有黑白像素。全白或全黑的图在屏幕上也是"一张图"，
		// 但扫不出任何东西——这里只做粗检，真正的解码验证在 e2e 里用 zbar 做。
		if dark, light := countPixels(t, body); dark == 0 || light == 0 {
			t.Errorf("%s：图是纯色的（黑 %d / 白 %d），肯定扫不出来", c.name, dark, light)
		}
	}
}

// countPixels 粗数一下深色/浅色像素，按 4 像素步长采样就够判断了
func countPixels(t *testing.T, data []byte) (dark, light int) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("解不开自己生成的 PNG: %v", err)
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y += 4 {
		for x := b.Min.X; x < b.Max.X; x += 4 {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r < 0x8000 && g < 0x8000 && bl < 0x8000 {
				dark++
			} else {
				light++
			}
		}
	}
	return dark, light
}

func TestQRRejectsBadInput(t *testing.T) {
	if rec := qrGet(""); rec.Code != http.StatusBadRequest {
		t.Errorf("空内容：状态码 = %d，期望 400", rec.Code)
	}

	// 边界：正好上限应通过，多一字节就拒绝
	if rec := qrGet(strings.Repeat("a", qrMaxLen)); rec.Code != http.StatusOK {
		t.Errorf("%d 字节（正好上限）：状态码 = %d，期望 200", qrMaxLen, rec.Code)
	}
	if rec := qrGet(strings.Repeat("a", qrMaxLen+1)); rec.Code != http.StatusBadRequest {
		t.Errorf("%d 字节（超一字节）：状态码 = %d，期望 400", qrMaxLen+1, rec.Code)
	}
}

func TestQRCacheable(t *testing.T) {
	text := "http://host:8080/f/1/x.pdf"
	a := qrGet(text)
	if cc := a.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q，期望含 immutable（内容由 URL 唯一决定）", cc)
	}

	// 长缓存的前提是确定性：同样的内容必须每次生成一模一样的图，
	// 否则浏览器缓存下来的和下次请求到的不是一张图，扫码结果会飘。
	b := qrGet(text)
	if !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
		t.Error("同一内容两次生成的图不一致，immutable 缓存就是在骗浏览器")
	}

	// 反过来，不同内容必须给出不同的图（防止"参数没接上、永远返回同一张"）
	c := qrGet("http://host:8080/f/1/y.pdf")
	if bytes.Equal(a.Body.Bytes(), c.Body.Bytes()) {
		t.Error("不同内容生成了完全相同的图，参数多半没接上")
	}
}

// 内容越长版本越高，图也应当越大——否则长 URL 会被挤到每模块不足一像素。
func TestQRSizeGrowsWithContent(t *testing.T) {
	short := qrGet("http://h/")
	long := qrGet("http://192.168.1.100:8080/f/abc123/" + strings.Repeat("%E5%B9%B4", 60) + ".pdf")
	if short.Code != http.StatusOK || long.Code != http.StatusOK {
		t.Fatalf("生成失败：short=%d long=%d", short.Code, long.Code)
	}

	shortCfg, err := png.DecodeConfig(bytes.NewReader(short.Body.Bytes()))
	if err != nil {
		t.Fatalf("读短内容尺寸失败: %v", err)
	}
	longCfg, err := png.DecodeConfig(bytes.NewReader(long.Body.Bytes()))
	if err != nil {
		t.Fatalf("读长内容尺寸失败: %v", err)
	}
	if longCfg.Width <= shortCfg.Width {
		t.Errorf("长内容的图（%dpx）没有比短内容（%dpx）大，长 URL 会糊掉",
			longCfg.Width, shortCfg.Width)
	}
	if longCfg.Width != longCfg.Height {
		t.Errorf("图不是正方形：%d×%d", longCfg.Width, longCfg.Height)
	}
}
