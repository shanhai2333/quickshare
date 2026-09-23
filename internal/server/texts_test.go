package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
	"unicode/utf8"

	"quickshare/internal/store"
)

// textTestWeb 是最小可用的内嵌前端资源。
//
// 里面那两行 `id="xxx" hidden` **必须跟 web/text.html 写得一模一样**：
// servePage 是按这个字面量去替换的，fixture 少一个空格就变成"测了个假契约"。
func textTestWeb() fstest.MapFS {
	const page = `<html lang="zh-CN"><head><link rel="stylesheet" href="/style.css"></head><body>`
	// 开了口令时这两个区块要默认隐藏（不能让未鉴权的人看到输入框），
	// 没开口令时由 servePage 把 hidden 摘掉——两件事都要能测到
	const gated = `<section id="composeCard" hidden></section><section id="textsCard" hidden></section>`
	// 空状态反过来：首帧必须隐藏，等数据回来再决定说不说
	const empty = `<div id="fileEmpty" hidden></div><div id="textEmpty" hidden></div>`
	return fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(page + empty + `<script src="/live.js"></script><script src="/app.js"></script></body></html>`)},
		"text.html":  &fstest.MapFile{Data: []byte(page + gated + empty + `<script src="/live.js"></script><script src="/text.js"></script></body></html>`)},
		"style.css":  &fstest.MapFile{Data: []byte("body{}")},
		"app.js":     &fstest.MapFile{Data: []byte("// app")},
		"live.js":    &fstest.MapFile{Data: []byte("// live")},
		"text.js":    &fstest.MapFile{Data: []byte("// text")},
	}
}

// newTextTestServer 起一个挂在临时数据库上的 Server，带最小可用的内嵌前端资源。
func newTextTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(Config{DataDir: t.TempDir()}, st, textTestWeb())
}

func doJSON(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 从响应里取一条文本列表
func decodeTexts(t *testing.T, rec *httptest.ResponseRecorder) []textDTO {
	t.Helper()
	var out []textDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析文本列表失败: %v（原文 %s）", err, rec.Body.String())
	}
	return out
}

func decodeDevices(t *testing.T, rec *httptest.ResponseRecorder) []deviceDTO {
	t.Helper()
	var out []deviceDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析设备列表失败: %v（原文 %s）", err, rec.Body.String())
	}
	return out
}

// ---------------------------------------------------------------- UA 解析

func TestDeviceNameFromUA(t *testing.T) {
	cases := []struct {
		name string
		ua   string
		want string
	}{
		{
			"iPhone Safari",
			"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
			"iPhone · Safari",
		},
		{
			"Android 手机 Chrome",
			"Mozilla/5.0 (Linux; Android 13; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Mobile Safari/537.36",
			"Android 手机 · Chrome",
		},
		{
			// 不带 Mobile 的按平板算，这是 UA 里区分两者的老约定
			"Android 平板 Chrome",
			"Mozilla/5.0 (Linux; Android 13; SM-X700) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36",
			"Android 平板 · Chrome",
		},
		{
			// 这条是重点：Edge 的 UA 里同时含 "Chrome" 和 "Safari"，
			// 判定顺序写反就会把它认成 Chrome。
			"Windows Edge 不能认成 Chrome",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36 Edg/119.0.0.0",
			"Windows · Edge",
		},
		{
			"Mac Chrome",
			"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36",
			"Mac · Chrome",
		},
		{"Linux 上的 curl", "curl/8.4.0", "curl"},
		{"空 UA", "", unknownDevice},
		{"认不出来的 UA", "SomethingWeird/1.0", unknownDevice},
	}
	for _, c := range cases {
		if got := deviceName(c.ua); got != c.want {
			t.Errorf("%s：deviceName = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- 工具函数

// deviceIDFromAddr 是设备身份的唯一来源（不经过代理头那条路），
// 形状判错会让"同一台机器"分裂或串台。
func TestClientIP(t *testing.T) {
	// 非本机地址：原样返回（去掉端口、规范化写法）
	remote := []struct {
		in   string
		want string
	}{
		{"198.51.100.5:54321", "198.51.100.5"},
		{"10.0.0.1:80", "10.0.0.1"},
		{"203.0.113.7:1234", "203.0.113.7"},
		// IPv6 的端口写在方括号外，SplitHostPort 会把括号一起去掉
		{"[2001:db8::1]:1234", "2001:db8::1"},
		// 解析不出来时原样返回，不 panic、不返回空
		{"没有端口", "没有端口"},
		{"", ""},
	}
	for _, c := range remote {
		r := &http.Request{RemoteAddr: c.in}
		if got := deviceIDFromAddr(r.RemoteAddr); got != c.want {
			t.Errorf("deviceIDFromAddr(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}

	// 回环：一律归一到同一个 ID。
	// 少了这一步，同一台机器换个地址打开就会变成两台设备。
	// 注意 127.0.0.2 也要归一——整个 127.0.0.0/8 都是回环，而测试脚本
	// 恰恰靠它来模拟"另一台设备"，判错会把测试自己搞乱。
	for _, in := range []string{"127.0.0.1:1234", "127.0.0.5:1234", "[::1]:1234"} {
		r := &http.Request{RemoteAddr: in}
		if got := deviceIDFromAddr(r.RemoteAddr); got != localDeviceID {
			t.Errorf("deviceIDFromAddr(%q) = %q，期望归一为 %q", in, got, localDeviceID)
		}
	}
}

// 本机网卡上的地址（不只是回环）也要归一——这正是"用内网 IP 打开又变成一台新
// 设备"的根因。地址因机器而异，所以从 InterfaceAddrs 动态取，不写死。
func TestClientIPLocalInterfaceNormalized(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("拿不到本机接口地址: %v", err)
	}
	checked := 0
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.String()
		remote := ip + ":1234"
		if strings.Contains(ip, ":") {
			remote = "[" + ip + "]:1234" // IPv6 在 RemoteAddr 里要带方括号
		}
		if got := deviceIDFromAddr(remote); got != localDeviceID {
			t.Errorf("本机地址 %s 应当归一为 %q，实际 %q", ip, localDeviceID, got)
		}
		checked++
	}
	if checked == 0 {
		t.Skip("这台机器没枚举到任何接口地址")
	}
}

// 网卡地址表的缓存必须能自愈。
//
// 服务可能在网卡拿到地址**之前**就起来了（NAS / 路由器上开机自启就是这个时序），
// 第一次查询于是缓存下一份不完整的列表。如果这份缓存是永久的（原来用 sync.Once
// 就是这样），之后用内网 IP 访问永远认不出是本机——同一台机器又分裂成两台设备，
// 而且不会自愈，只能重启。
//
// 这里手工把缓存灌成"空表、且刚刷新过"，再问一个真实存在的本机地址：
// 必须能查出来。灌"刚刷新过"是为了卡住保鲜期那条路径，逼着实现靠
// "没命中就重扫"自愈——否则这个测试在 TTL 到期后才碰巧变绿。
func TestIsLocalAddrRecoversFromStaleCache(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("拿不到本机接口地址: %v", err)
	}
	var want string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue // 优先挑 IPv4，比较稳
		}
		want = ipnet.IP.String()
		break
	}
	if want == "" {
		t.Skip("本机没有非回环的 IPv4 接口地址")
	}

	localAddrsMu.Lock()
	localAddrsSet = map[string]bool{}
	localAddrsAt = time.Now()
	localAddrsMu.Unlock()

	if !isLocalAddr(want) {
		t.Errorf("缓存里没有 %s，应当重扫一次并认出它是本机地址；"+
			"说明缓存变成了永久快照，开机时序错了就再也不会自愈", want)
	}
}

func TestCutUTF8NeverSplitsRune(t *testing.T) {
	// 每个汉字 3 字节，随便取几个截断点，都不该切出半个字符
	s := strings.Repeat("中文测试", 20)
	for n := 0; n <= len(s); n++ {
		got := cutUTF8(s, n)
		if len(got) > n {
			t.Fatalf("截断到 %d 字节后长度 %d，超了", n, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("截断到 %d 字节后不是合法 UTF-8: %q", n, got)
		}
	}
	if got := cutUTF8("short", 100); got != "short" {
		t.Fatalf("没超限时不该改动，得到 %q", got)
	}
}

// ---------------------------------------------------------------- 接口

func TestTextAPIEndToEnd(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	const ua = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"

	// 建一条
	req := httptest.NewRequest(http.MethodPost, "/api/texts",
		strings.NewReader(`{"content":"你好，这是一条测试"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("新建文本状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var created textDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析新建结果失败: %v", err)
	}
	if created.ID == "" {
		t.Fatal("新建的文本没有 ID")
	}
	// 服务端应当顺手把发送方记进设备表，并按 UA 解析出显示名
	if created.DeviceName != "iPhone · Safari" {
		t.Errorf("DeviceName = %q，期望 %q", created.DeviceName, "iPhone · Safari")
	}

	// 列表
	rec = doJSON(t, h, http.MethodGet, "/api/texts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("列表状态码 = %d，期望 200", rec.Code)
	}
	texts := decodeTexts(t, rec)
	if len(texts) != 1 || texts[0].Content != "你好，这是一条测试" {
		t.Fatalf("列表内容不对: %+v", texts)
	}

	// 改内容
	rec = doJSON(t, h, http.MethodPut, "/api/texts/"+created.ID, `{"content":"改过了"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("修改状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var updated textDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("解析修改结果失败: %v", err)
	}
	if updated.Content != "改过了" {
		t.Errorf("改完内容 = %q，期望 %q", updated.Content, "改过了")
	}
	if updated.UpdatedAt < updated.CreatedAt {
		t.Errorf("updatedAt(%d) 不该早于 createdAt(%d)", updated.UpdatedAt, updated.CreatedAt)
	}

	// 改不存在的 ID：必须 404，不能回 200 让前端以为改成功了
	rec = doJSON(t, h, http.MethodPut, "/api/texts/no-such-id", `{"content":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("改不存在的文本状态码 = %d，期望 404", rec.Code)
	}

	// 删
	rec = doJSON(t, h, http.MethodDelete, "/api/texts/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除状态码 = %d，期望 200", rec.Code)
	}
	// 再删一次应当 404
	rec = doJSON(t, h, http.MethodDelete, "/api/texts/"+created.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("重复删除状态码 = %d，期望 404", rec.Code)
	}

	rec = doJSON(t, h, http.MethodGet, "/api/texts", "")
	if got := decodeTexts(t, rec); len(got) != 0 {
		t.Fatalf("删完列表还有 %d 条", len(got))
	}
}

func TestCreateTextRejectsBadInput(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	cases := []struct {
		name string
		body string
		want int
	}{
		{"空内容", `{"content":""}`, http.StatusBadRequest},
		{"全是空白", `{"content":"  \n\t "}`, http.StatusBadRequest},
		{"超长内容", `{"content":"` + strings.Repeat("x", maxTextLen+1) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		rec := doJSON(t, h, http.MethodPost, "/api/texts", c.body)
		if rec.Code != c.want {
			t.Errorf("%s：状态码 = %d，期望 %d（%s）", c.name, rec.Code, c.want, rec.Body.String())
		}
	}

	// 请求体里的 deviceId 现在完全不被采信：塞一个别人的地址进去也不会生效。
	// 这正是要的——设备身份只能由服务端从连接推导，否则谁都能冒充别人的设备。
	rec := doJSON(t, h, http.MethodPost, "/api/texts",
		`{"content":"伪造","deviceId":"1.2.3.4"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("带伪造 deviceId 应当照常成功，状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var got textDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	// httptest.NewRequest 给的默认来源是 192.0.2.1:1234
	if got.DeviceID != "192.0.2.1" {
		t.Errorf("设备 ID = %q，期望取自连接来源 192.0.2.1", got.DeviceID)
	}
}

// 设备身份 = 来源 IP。这条守住本次改动的核心目的：同一台机器不论用哪个地址、
// 哪个浏览器打开都必须归到同一台设备，不能因为换了个入口就多出一条。
func TestDevicesKeyedByClientIP(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	post := func(ip string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/texts",
			strings.NewReader(`{"content":"来自 `+ip+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = ip + ":40000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("发文本状态码 = %d（%s）", rec.Code, rec.Body.String())
		}
	}

	// 同一台机器反复发（模拟换地址、换窗口、换浏览器）：仍然只有一台
	post("198.51.100.10")
	post("198.51.100.10")
	post("198.51.100.10")

	devices := decodeDevices(t, doJSON(t, h, http.MethodGet, "/api/devices", ""))
	if len(devices) != 1 {
		t.Fatalf("同一 IP 发了 3 次，期望 1 台设备，实际 %d 台", len(devices))
	}
	if devices[0].ID != "198.51.100.10" {
		t.Errorf("设备 ID = %q，期望 %q", devices[0].ID, "198.51.100.10")
	}

	// 换一台机器：多出一条
	post("198.51.100.20")
	devices = decodeDevices(t, doJSON(t, h, http.MethodGet, "/api/devices", ""))
	if len(devices) != 2 {
		t.Fatalf("换了台机器后期望 2 台设备，实际 %d 台", len(devices))
	}
}

// isMe 由服务端按来源算：同一份列表，不同请求方看到的"本机"不是同一台。
func TestDeviceIsMe(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	post := func(ip string) {
		req := httptest.NewRequest(http.MethodPost, "/api/texts",
			strings.NewReader(`{"content":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = ip + ":40000"
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	post("198.51.100.10")
	post("198.51.100.20")

	list := func(ip string) []deviceDTO {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
		req.RemoteAddr = ip + ":40000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return decodeDevices(t, rec)
	}

	for _, me := range []string{"198.51.100.10", "198.51.100.20"} {
		devices := list(me)
		if len(devices) != 2 {
			t.Fatalf("期望 2 台设备，实际 %d 台", len(devices))
		}
		for _, d := range devices {
			want := d.ID == me
			if d.IsMe != want {
				t.Errorf("以 %s 的身份看：设备 %s 的 isMe = %v，期望 %v",
					me, d.ID, d.IsMe, want)
			}
		}
	}
}

// 内容首尾的空白是有意义的（贴代码块时尤其明显），服务端不能替用户抹掉。
func TestCreateTextKeepsWhitespace(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	const content = "\n    indented();\n\n"
	rec := doJSON(t, h, http.MethodPost, "/api/texts",
		`{"content":`+jsonString(content)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("新建状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, h, http.MethodGet, "/api/texts", "")
	texts := decodeTexts(t, rec)
	if len(texts) != 1 {
		t.Fatalf("期望 1 条，实际 %d 条", len(texts))
	}
	if texts[0].Content != content {
		t.Errorf("内容被改动了：%q，期望 %q", texts[0].Content, content)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ---------------------------------------------------------------- 设备备注

func TestDeviceRemark(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36"
	// 设备身份就是来源 IP，所以"是哪台设备"由 RemoteAddr 决定。
	// 用 RFC 5737 的文档地址，免得撞上某台机器真实存在的网卡。
	const dev = "198.51.100.30"

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/texts",
			strings.NewReader(`{"content":"一条文本"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", ua)
		req.RemoteAddr = dev + ":5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := post(); rec.Code != http.StatusOK {
		t.Fatalf("新建状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, h, http.MethodGet, "/api/devices", "")
	devices := decodeDevices(t, rec)
	if len(devices) != 1 {
		t.Fatalf("期望 1 台设备，实际 %d 台", len(devices))
	}
	if devices[0].Name != "Windows · Chrome" {
		t.Errorf("默认设备名 = %q，期望 %q", devices[0].Name, "Windows · Chrome")
	}

	// 改备注
	rec = doJSON(t, h, http.MethodPut, "/api/devices/"+dev, `{"remark":"老王的电脑"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("改备注状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	// 文本列表里显示的名字要跟着变
	rec = doJSON(t, h, http.MethodGet, "/api/texts", "")
	texts := decodeTexts(t, rec)
	if len(texts) != 1 || texts[0].DeviceName != "老王的电脑" {
		t.Fatalf("改完备注后文本里的设备名没变: %+v", texts)
	}

	// **关键**：再发一条，TouchDevice 不能把备注覆盖回空。
	// upsert 的 DO UPDATE 里顺手写 remark 就会踩这个坑，而且只在"改完备注
	// 又发了东西"之后才暴露。
	if rec := post(); rec.Code != http.StatusOK {
		t.Fatalf("再发一条状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodGet, "/api/devices", "")
	devices = decodeDevices(t, rec)
	if len(devices) != 1 || devices[0].Remark != "老王的电脑" {
		t.Fatalf("再发一条之后备注丢了: %+v", devices)
	}

	// 清空备注 -> 回到 UA 解析出来的名字
	rec = doJSON(t, h, http.MethodPut, "/api/devices/"+dev, `{"remark":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空备注状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodGet, "/api/devices", "")
	devices = decodeDevices(t, rec)
	if devices[0].Name != "Windows · Chrome" {
		t.Errorf("清空备注后名字 = %q，期望回到 %q", devices[0].Name, "Windows · Chrome")
	}

	// 改不存在的设备
	rec = doJSON(t, h, http.MethodPut, "/api/devices/no-such-dev", `{"remark":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("改不存在设备的状态码 = %d，期望 404", rec.Code)
	}
}

// postTextFrom 以指定源地址发一条文本。
//
// 设备身份就是源地址，所以"造出第二台设备"只能在 RemoteAddr 上做文章
// （请求体里的 deviceId 从改版起就不起作用了）。地址用 RFC 5737 的文档段。
func postTextFrom(t *testing.T, h http.Handler, ip, content string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/texts",
		strings.NewReader(`{"content":"`+content+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("从 %s 发文本状态码 = %d（%s）", ip, rec.Code, rec.Body.String())
	}
}

// textIDByContent 按内容找一条文本的 ID。
func textIDByContent(t *testing.T, h http.Handler, content string) string {
	t.Helper()
	for _, tx := range decodeTexts(t, doJSON(t, h, http.MethodGet, "/api/texts", "")) {
		if tx.Content == content {
			return tx.ID
		}
	}
	t.Fatalf("找不到内容为 %q 的文本", content)
	return ""
}

// 删设备只删记录，文本要留着（只是显示名回落成「未知设备」）。
func TestDeleteDeviceAPI(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	postTextFrom(t, h, "198.51.100.41", "甲的一条")
	postTextFrom(t, h, "198.51.100.42", "乙的一条")

	// 先给要删的那台起个名字，确认备注也一起没了
	if rec := doJSON(t, h, http.MethodPut,
		"/api/devices/198.51.100.41", `{"remark":"甲的手机"}`); rec.Code != http.StatusOK {
		t.Fatalf("设备注状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, h, http.MethodDelete, "/api/devices/198.51.100.41", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("删设备状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	devices := decodeDevices(t, doJSON(t, h, http.MethodGet, "/api/devices", ""))
	if len(devices) != 1 || devices[0].ID != "198.51.100.42" {
		t.Fatalf("删完应当只剩乙那台，实际 %+v", devices)
	}

	// **文本必须都还在**：用户点的是"把这个设备条目去掉"，不是"清空它的内容"
	texts := decodeTexts(t, doJSON(t, h, http.MethodGet, "/api/texts", ""))
	if len(texts) != 2 {
		t.Fatalf("删设备不该动文本，期望 2 条，实际 %d 条", len(texts))
	}
	for _, tx := range texts {
		if tx.Content == "甲的一条" && tx.DeviceName != unknownDevice {
			t.Errorf("设备记录被删后显示名 = %q，期望 %q", tx.DeviceName, unknownDevice)
		}
	}

	// 重复删 -> 404（不能对着不存在的记录回 200）
	rec = doJSON(t, h, http.MethodDelete, "/api/devices/198.51.100.41", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("重复删除的状态码 = %d，期望 404", rec.Code)
	}
}

// 「设备随消息删除」默认**关闭**：删了文本，设备记录要留着。
//
// 留着是有用的——用户给一台设备起好的名字，下次它再发文本时还在。
func TestPruneDevicesOffByDefault(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	postTextFrom(t, h, "198.51.100.51", "唯一一条")
	id := textIDByContent(t, h, "唯一一条")
	if rec := doJSON(t, h, http.MethodDelete, "/api/texts/"+id, ""); rec.Code != http.StatusOK {
		t.Fatalf("删文本状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	devices := decodeDevices(t, doJSON(t, h, http.MethodGet, "/api/devices", ""))
	if len(devices) != 1 {
		t.Fatalf("默认不该清设备记录，实际剩 %d 台", len(devices))
	}
	if devices[0].TextCount != 0 {
		t.Errorf("文本删光了，textCount = %d，期望 0", devices[0].TextCount)
	}
}

// 打开开关之后，删文本会顺手带走"已经一条不剩"的设备记录。
func TestPruneDevicesAfterDelete(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	postTextFrom(t, h, "198.51.100.61", "甲的两条之一")
	postTextFrom(t, h, "198.51.100.61", "甲的两条之二")
	postTextFrom(t, h, "198.51.100.62", "乙的一条")

	if rec := doJSON(t, h, http.MethodPut, "/api/settings",
		`{"pruneDevices":true}`); rec.Code != http.StatusOK {
		t.Fatalf("打开开关状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	// 甲还剩一条，删掉它之后甲才该消失
	first := textIDByContent(t, h, "甲的两条之一")
	if rec := doJSON(t, h, http.MethodDelete, "/api/texts/"+first, ""); rec.Code != http.StatusOK {
		t.Fatalf("删第一条状态码 = %d", rec.Code)
	}
	devices := decodeDevices(t, doJSON(t, h, http.MethodGet, "/api/devices", ""))
	if len(devices) != 2 {
		t.Fatalf("甲还剩一条文本，不该被清掉，实际剩 %d 台", len(devices))
	}

	second := textIDByContent(t, h, "甲的两条之二")
	if rec := doJSON(t, h, http.MethodDelete, "/api/texts/"+second, ""); rec.Code != http.StatusOK {
		t.Fatalf("删第二条状态码 = %d", rec.Code)
	}
	devices = decodeDevices(t, doJSON(t, h, http.MethodGet, "/api/devices", ""))
	if len(devices) != 1 || devices[0].ID != "198.51.100.62" {
		t.Fatalf("甲已经空了，应当只剩乙，实际 %+v", devices)
	}

	// 批量删除这条路也要生效
	last := textIDByContent(t, h, "乙的一条")
	if rec := doJSON(t, h, http.MethodPost, "/api/texts/delete",
		`{"ids":["`+last+`"]}`); rec.Code != http.StatusOK {
		t.Fatalf("批量删除状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if devices = decodeDevices(t, doJSON(t, h, http.MethodGet, "/api/devices", "")); len(devices) != 0 {
		t.Fatalf("全删光之后设备也该空了，实际剩 %+v", devices)
	}
}

// 打开开关的**那一瞬间**，已经空掉的设备记录要立刻清掉。
//
// 不这么做的话，用户打开开关、回到列表一看还是那一堆空设备，会以为没生效
// ——而它其实只对"以后"的删除生效。
func TestPruneDevicesOnEnableClearsExisting(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	postTextFrom(t, h, "198.51.100.71", "先有的一条")
	postTextFrom(t, h, "198.51.100.72", "要留着的一条")

	// 开关是关的，所以删光 71 的文本后它还挂在设备列表里
	id := textIDByContent(t, h, "先有的一条")
	if rec := doJSON(t, h, http.MethodDelete, "/api/texts/"+id, ""); rec.Code != http.StatusOK {
		t.Fatalf("删文本状态码 = %d", rec.Code)
	}
	if devices := decodeDevices(t, doJSON(t, h, http.MethodGet, "/api/devices", "")); len(devices) != 2 {
		t.Fatalf("开关没开时不该清设备，期望 2 台，实际 %d 台", len(devices))
	}

	// 打开开关 -> 空的那台立刻消失，还有文本的那台留下
	if rec := doJSON(t, h, http.MethodPut, "/api/settings",
		`{"pruneDevices":true}`); rec.Code != http.StatusOK {
		t.Fatalf("打开开关状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	devices := decodeDevices(t, doJSON(t, h, http.MethodGet, "/api/devices", ""))
	if len(devices) != 1 || devices[0].ID != "198.51.100.72" {
		t.Fatalf("打开开关应当立刻清掉空设备，实际 %+v", devices)
	}
}

// 开关要在设置接口里能读能写，默认关闭。
func TestPruneDevicesSettingRoundTrip(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	read := func() bool {
		t.Helper()
		rec := doJSON(t, h, http.MethodGet, "/api/settings", "")
		var kv map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &kv); err != nil {
			t.Fatalf("解析设置失败: %v（原文 %s）", err, rec.Body.String())
		}
		v, ok := kv["pruneDevices"].(bool)
		if !ok {
			t.Fatalf("pruneDevices 不是布尔值: %#v", kv["pruneDevices"])
		}
		return v
	}

	if read() {
		t.Error("默认应当是关闭的")
	}
	if rec := doJSON(t, h, http.MethodPut, "/api/settings",
		`{"pruneDevices":true}`); rec.Code != http.StatusOK {
		t.Fatalf("写开关状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if !read() {
		t.Error("写完之后读回应当是开启的")
	}
	if rec := doJSON(t, h, http.MethodPut, "/api/settings",
		`{"pruneDevices":false}`); rec.Code != http.StatusOK {
		t.Fatalf("关开关状态码 = %d", rec.Code)
	}
	if read() {
		t.Error("关掉之后读回应当是关闭的")
	}

	// 只改别的设置不能把开关带跑（字段是可选的，没传就不动）
	if rec := doJSON(t, h, http.MethodPut, "/api/settings",
		`{"pruneDevices":true}`); rec.Code != http.StatusOK {
		t.Fatalf("再打开状态码 = %d", rec.Code)
	}
	if rec := doJSON(t, h, http.MethodPut, "/api/settings",
		`{"theme":"light"}`); rec.Code != http.StatusOK {
		t.Fatalf("只改主题状态码 = %d", rec.Code)
	}
	if !read() {
		t.Error("只改主题不该把设备清理开关关掉")
	}
}

func TestChunkSizeSettingRoundTrip(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()
	read := func() int64 {
		t.Helper()
		var out map[string]any
		rec := doJSON(t, h, http.MethodGet, "/api/settings", "")
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析设置失败: %v", err)
		}
		return int64(out["chunkSize"].(float64))
	}
	if got := read(); got != 8<<20 {
		t.Fatalf("默认分片大小 = %d，期望 %d", got, 8<<20)
	}
	if rec := doJSON(t, h, http.MethodPut, "/api/settings", `{"chunkSize":4194304}`); rec.Code != http.StatusOK {
		t.Fatalf("设置 4 MiB 状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if got := read(); got != 4<<20 {
		t.Fatalf("设置后分片大小 = %d，期望 %d", got, 4<<20)
	}
	for _, n := range []int{0, 65 << 20} {
		body := fmt.Sprintf(`{"chunkSize":%d}`, n)
		if rec := doJSON(t, h, http.MethodPut, "/api/settings", body); rec.Code != http.StatusBadRequest {
			t.Errorf("非法分片大小 %d 状态码 = %d，期望 400", n, rec.Code)
		}
	}
}

// 超长 UA 入库要被截断，且截断处不能切碎字符。
func TestLongUATruncated(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	ua := strings.Repeat("设备", 400) // 2400 字节，且都是多字节字符
	req := httptest.NewRequest(http.MethodPost, "/api/texts",
		strings.NewReader(`{"content":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("新建状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, h, http.MethodGet, "/api/devices", "")
	devices := decodeDevices(t, rec)
	if len(devices) != 1 {
		t.Fatalf("期望 1 台设备，实际 %d 台", len(devices))
	}
	if len(devices[0].UA) > maxUALen {
		t.Errorf("UA 入库长度 %d，超过上限 %d", len(devices[0].UA), maxUALen)
	}
	if !utf8.ValidString(devices[0].UA) {
		t.Errorf("截断后的 UA 不是合法 UTF-8: %q", devices[0].UA)
	}
}

// ---------------------------------------------------------------- 推送

func TestTextChangeNotifiesTextsTopic(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	sub, cancel := s.events.subscribe()
	defer cancel()

	rec := doJSON(t, h, http.MethodPost, "/api/texts", `{"content":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("新建状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if !waitTopic(t, sub, topicTexts, time.Second) {
		t.Fatal("新建文本之后没收到 texts 主题的通知")
	}

	// 再确认一次：文件主题不该被这个动作带上
	select {
	case <-sub.wake:
		for _, got := range sub.drain() {
			if got == topicFiles {
				t.Fatal("发文本不该触发 files 主题")
			}
		}
	case <-time.After(50 * time.Millisecond):
		// 预期：没有新通知
	}
}

// ---------------------------------------------------------------- 页面

func TestTextPageInjectsThemeAndAssetVersion(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	rec := doJSON(t, h, http.MethodPut, "/api/settings", `{"theme":"dark"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("写主题状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, h, http.MethodGet, "/text", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("文本页状态码 = %d，期望 200", rec.Code)
	}
	page := rec.Body.String()
	for _, want := range []string{
		`data-theme="dark"`, // 首帧配色，不能等 JS
		"/style.css?v=",
		"/live.js?v=",
		"/text.js?v=",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("文本页 HTML 里缺少 %q", want)
		}
	}
	if strings.Contains(page, `href="/style.css"`) {
		t.Error("style.css 没有被替换成带版本号的地址")
	}

	// 首页同样要带上 live.js 的版本号
	rec = doJSON(t, h, http.MethodGet, "/", "")
	home := rec.Body.String()
	for _, want := range []string{`data-theme="dark"`, "/app.js?v=", "/live.js?v="} {
		if !strings.Contains(home, want) {
			t.Errorf("首页 HTML 里缺少 %q", want)
		}
	}
}

// 没设访问口令时（内网自用的默认情况），文本页那两个区块的首帧状态必须是"可见"。
//
// 它们在 HTML 里默认 hidden，是为了"开了口令时不能让未鉴权的人看到输入框"；
// 但没开口令时如果不摘掉，它们要等 JS 跑完「config → settings → 三个并行请求」
// 才露面。实测（注入 120ms 延迟模拟 NAS + WiFi）：顶栏 157ms 就画出来了，
// 主区一直空到 525ms，**空白 368ms**；而首页的卡片本来就没带 hidden，
// 切回去反而不空白——这个不对称就是"切页闪一下"的主因。
func TestTextPageShowsCardsWhenNoAuth(t *testing.T) {
	s := newTextTestServer(t)
	rec := doJSON(t, s.Handler(), http.MethodGet, "/text", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("文本页状态码 = %d，期望 200", rec.Code)
	}
	page := rec.Body.String()

	for _, id := range []string{"composeCard", "textsCard"} {
		if strings.Contains(page, `id="`+id+`" hidden`) {
			t.Errorf("没开口令时 #%s 上还挂着 hidden —— 首帧会空一大片主区", id)
		}
		// 顺便确认"没匹配上"不是因为我们把 id 写错了（那样上面那条会假绿）
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("文本页里找不到 #%s —— 是不是改了 id", id)
		}
	}

	// 空状态反过来：首帧**不能**显示。数据还没回来就说"还没有文本"，
	// 是在报一个还不知道真假的结论；真有文本时会先闪一下这句再被列表替掉。
	if !strings.Contains(page, `id="textEmpty" hidden`) {
		t.Error("文本页 #textEmpty 没有 hidden —— 有文本时会先闪一下这句话")
	}
	if !strings.Contains(rec.Body.String(), `id="textEmpty"`) {
		t.Error("文本页里找不到 #textEmpty")
	}
}

// 开了口令时，那两个 hidden **必须留着**：没鉴权的人不该看到输入框。
// 方向搞反的话，未鉴权的人会先看到输入框、再被换成口令卡片。
func TestTextPageKeepsCardsHiddenWhenAuthRequired(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	s := New(Config{DataDir: t.TempDir(), AdminToken: "secret"}, st, textTestWeb())
	rec := doJSON(t, s.Handler(), http.MethodGet, "/text", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("文本页状态码 = %d，期望 200", rec.Code)
	}
	page := rec.Body.String()
	for _, id := range []string{"composeCard", "textsCard"} {
		if !strings.Contains(page, `id="`+id+`" hidden`) {
			t.Errorf("开口令时 #%s 的 hidden 被摘掉了 —— 未鉴权的人会看到内容", id)
		}
	}
}

// ---------------------------------------------------------------- 批量删除

func TestBatchDeleteAPI(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	var ids []string
	for _, content := range []string{"第一条", "第二条", "第三条"} {
		rec := doJSON(t, h, http.MethodPost, "/api/texts",
			`{"content":"`+content+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("新建状态码 = %d（%s）", rec.Code, rec.Body.String())
		}
		var got textDTO
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		ids = append(ids, got.ID)
	}

	deletedOf := func(rec *httptest.ResponseRecorder) int {
		t.Helper()
		var out struct {
			Deleted int `json:"deleted"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析结果失败: %v（%s）", err, rec.Body.String())
		}
		return out.Deleted
	}

	// 删前两条
	body, _ := json.Marshal(map[string]any{"ids": ids[:2]})
	rec := doJSON(t, h, http.MethodPost, "/api/texts/delete", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("批量删除状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if got := deletedOf(rec); got != 2 {
		t.Errorf("deleted = %d，期望 2", got)
	}
	left := decodeTexts(t, doJSON(t, h, http.MethodGet, "/api/texts", ""))
	if len(left) != 1 {
		t.Fatalf("删完剩 %d 条，期望 1", len(left))
	}

	// 混入不存在的 ID：照常删掉存在的那些，不报错（多选时某条被别处删掉是正常的）
	body, _ = json.Marshal(map[string]any{"ids": []string{left[0].ID, "no-such-id"}})
	rec = doJSON(t, h, http.MethodPost, "/api/texts/delete", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("含不存在 ID 时状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	if got := deletedOf(rec); got != 1 {
		t.Errorf("deleted = %d，期望 1", got)
	}

	// 空列表与超量都要被拒
	if code := doJSON(t, h, http.MethodPost, "/api/texts/delete", `{"ids":[]}`).Code; code != http.StatusBadRequest {
		t.Errorf("空 ids 状态码 = %d，期望 400", code)
	}
	tooMany := make([]string, maxBatchDelete+1)
	for i := range tooMany {
		tooMany[i] = "x"
	}
	body, _ = json.Marshal(map[string]any{"ids": tooMany})
	if code := doJSON(t, h, http.MethodPost, "/api/texts/delete", string(body)).Code; code != http.StatusBadRequest {
		t.Errorf("超量 ids 状态码 = %d，期望 400", code)
	}
}

// ---------------------------------------------------------------- 保留时长

func TestTextTTLSettings(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	ttlOf := func(rec *httptest.ResponseRecorder) (int, string) {
		t.Helper()
		var kv map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &kv); err != nil {
			t.Fatalf("解析设置失败: %v（%s）", err, rec.Body.String())
		}
		var ttl struct {
			Value int    `json:"value"`
			Unit  string `json:"unit"`
		}
		if err := json.Unmarshal(kv["textTTL"], &ttl); err != nil {
			t.Fatalf("解析 textTTL 失败: %v（%s）", err, string(kv["textTTL"]))
		}
		return ttl.Value, ttl.Unit
	}

	// 默认 0 / day —— 0 表示不自动删除
	if v, u := ttlOf(doJSON(t, h, http.MethodGet, "/api/settings", "")); v != 0 || u != "day" {
		t.Fatalf("默认 textTTL = %d/%s，期望 0/day", v, u)
	}

	rec := doJSON(t, h, http.MethodPut, "/api/settings", `{"textTTL":{"value":6,"unit":"hour"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("写设置状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if v, u := ttlOf(rec); v != 6 || u != "hour" {
		t.Fatalf("写回 = %d/%s，期望 6/hour", v, u)
	}

	for _, body := range []string{
		`{"textTTL":{"value":-1}}`,
		`{"textTTL":{"value":10001}}`,
		`{"textTTL":{"unit":"week"}}`,
	} {
		if code := doJSON(t, h, http.MethodPut, "/api/settings", body).Code; code != http.StatusBadRequest {
			t.Errorf("%s 状态码 = %d，期望 400", body, code)
		}
	}
	// 被拒的值不能污染已经存下的
	if v, u := ttlOf(doJSON(t, h, http.MethodGet, "/api/settings", "")); v != 6 || u != "hour" {
		t.Fatalf("被拒后设置变了：%d/%s，期望仍是 6/hour", v, u)
	}

	// 只改 value 不改 unit：单位应当保持不变
	doJSON(t, h, http.MethodPut, "/api/settings", `{"textTTL":{"value":12}}`)
	if v, u := ttlOf(doJSON(t, h, http.MethodGet, "/api/settings", "")); v != 12 || u != "hour" {
		t.Fatalf("只改 value 后 = %d/%s，期望 12/hour", v, u)
	}
}

func TestTextTTLFrom(t *testing.T) {
	val := store.SettingTextTTLValue
	unit := store.SettingTextTTLUnit

	cases := []struct {
		name string
		kv   map[string]string
		want time.Duration
		ok   bool
	}{
		{"完全没设", map[string]string{}, 0, false},
		{"值为空串", map[string]string{val: ""}, 0, false},
		{"值为 0（关闭）", map[string]string{val: "0", unit: "day"}, 0, false},
		{"值为负", map[string]string{val: "-3", unit: "day"}, 0, false},
		{"值不是数字", map[string]string{val: "abc", unit: "day"}, 0, false},
		{"3 天", map[string]string{val: "3", unit: "day"}, 72 * time.Hour, true},
		{"6 小时", map[string]string{val: "6", unit: "hour"}, 6 * time.Hour, true},
		{"缺单位时按天算", map[string]string{val: "2"}, 48 * time.Hour, true},
		// 认不出的单位宁可不清理：库里的值被手改脏时，不能顺手把用户的文本删了
		{"单位认不出就不动数据", map[string]string{val: "2", unit: "week"}, 0, false},
	}
	for _, c := range cases {
		got, ok := textTTLFrom(c.kv)
		if ok != c.ok || got != c.want {
			t.Errorf("%s：得到 %v/%v，期望 %v/%v", c.name, got, c.ok, c.want, c.ok)
		}
	}
}

// ---------------------------------------------------------------- 定时清理

func TestCleanupPurgesExpiredTexts(t *testing.T) {
	s := newTextTestServer(t)
	h := s.Handler()

	now := time.Now()
	mk := func(id, content string, created time.Time) {
		t.Helper()
		if err := s.be().st.CreateText(&store.Text{
			ID: id, Content: content, DeviceID: "dev-1",
			CreatedAt: created, UpdatedAt: created,
		}); err != nil {
			t.Fatalf("造数据 %s 失败: %v", id, err)
		}
	}
	mk("old", "两天前的", now.Add(-48*time.Hour))
	mk("fresh", "刚发的", now)

	// 没设保留时长时，清理不该动任何东西——这是默认行为
	s.runCleanup()
	if got := len(decodeTexts(t, doJSON(t, h, http.MethodGet, "/api/texts", ""))); got != 2 {
		t.Fatalf("未设置保留时长时被删了，剩 %d 条，期望 2", got)
	}

	if rec := doJSON(t, h, http.MethodPut, "/api/settings",
		`{"textTTL":{"value":1,"unit":"day"}}`); rec.Code != http.StatusOK {
		t.Fatalf("写设置状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	// 先订阅再清理：notify 是同步的，晚订阅就收不到这一次通知
	sub, cancel := s.events.subscribe()
	defer cancel()

	s.runCleanup()

	left := decodeTexts(t, doJSON(t, h, http.MethodGet, "/api/texts", ""))
	if len(left) != 1 || left[0].ID != "fresh" {
		t.Fatalf("清理后剩 %d 条，期望只剩 fresh", len(left))
	}
	// 清理掉东西也要推通知，否则打开的页面会一直挂着已经不在的条目
	if !waitTopic(t, sub, topicTexts, time.Second) {
		t.Fatal("清理掉文本之后没推 texts 主题")
	}
}

// 上面那个测试直接调 runCleanup()，只覆盖了"清理逻辑本身"。
// 真实部署里跑的是 StartCleanup 起的 goroutine + ticker，那段接线（以及 stop）
// 是另一个东西——周期填错、忘了循环、stop 关不掉，都会让自动清理在线上失效，
// 而单测全绿。这里把真实的 ticker 路径跑一遍。
func TestStartCleanupTickerPurges(t *testing.T) {
	s := newTextTestServer(t)

	if err := s.be().st.CreateText(&store.Text{
		ID: "old", Content: "两天前的", DeviceID: "dev-1",
		CreatedAt: time.Now().Add(-48 * time.Hour),
		UpdatedAt: time.Now().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("造数据失败: %v", err)
	}
	// 保留 1 天 -> 上面那条 48 小时前的应该被清掉
	if err := s.be().st.SetSetting(store.SettingTextTTLValue, "1"); err != nil {
		t.Fatalf("写保留时长失败: %v", err)
	}
	if err := s.be().st.SetSetting(store.SettingTextTTLUnit, "day"); err != nil {
		t.Fatalf("写保留单位失败: %v", err)
	}

	// 周期给得极短，让 ticker 真的转起来（StartCleanup 在等第一个 tick 之前
	// 也会先跑一次 runCleanup，所以这里两条路径都能走到）
	stop := s.StartCleanup(20 * time.Millisecond)
	defer stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		left, err := s.be().st.ListTexts()
		if err != nil {
			t.Fatalf("读文本列表失败: %v", err)
		}
		if len(left) == 0 {
			return // 被定时任务清掉了
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("StartCleanup 起的定时任务没有清掉过期文本")
}

// stop() 之后不该再有清理发生——关服务时还在后台删数据，会和进程退出抢文件。
//
// 这条同时盯住 stop() 的**同步语义**：它返回时 goroutine 必须已经退出。
// 早先 stop() 只 close(stop) 就返回，正在跑的那一轮 runCleanup 还会继续，
// 于是关库时稳定吐一行 "database is closed"。那时这里得靠 sleep 硬等，
// 现在是 stop() 自己保证的，不用等。
func TestStartCleanupStopHalts(t *testing.T) {
	s := newTextTestServer(t)

	stop := s.StartCleanup(20 * time.Millisecond)
	time.Sleep(60 * time.Millisecond) // 让它先转几轮，确认 goroutine 起来了
	stop()
	stop() // 停两次不该 panic（close 一个已关闭的 channel 会 panic）

	if err := s.be().st.CreateText(&store.Text{
		ID: "old", Content: "两天前的", DeviceID: "dev-1",
		CreatedAt: time.Now().Add(-48 * time.Hour),
		UpdatedAt: time.Now().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("造数据失败: %v", err)
	}
	if err := s.be().st.SetSetting(store.SettingTextTTLValue, "1"); err != nil {
		t.Fatalf("写保留时长失败: %v", err)
	}
	if err := s.be().st.SetSetting(store.SettingTextTTLUnit, "day"); err != nil {
		t.Fatalf("写保留单位失败: %v", err)
	}

	// 等远超一个周期的时长：真停了的话，这条过期文本必须还在
	time.Sleep(300 * time.Millisecond)
	left, err := s.be().st.ListTexts()
	if err != nil {
		t.Fatalf("读文本列表失败: %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("stop() 之后仍在清理，剩 %d 条，期望 1", len(left))
	}
}
