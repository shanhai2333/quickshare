package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quickshare/internal/store"
	"quickshare/internal/version"
)

// setVersion 临时改掉全局版本号。version.Version 是包级变量（构建时注入的），
// 测试里只能这么改；用 t.Cleanup 还原，免得污染后面的用例。
func setVersion(t *testing.T, v string) {
	t.Helper()
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

const releaseJSON = `{"tag_name":"v2.0.0","html_url":"https://example.com/rel","published_at":"2026-09-01T00:00:00Z"}`

// fakeGitHub 起一个假的 GitHub API。hits 用来断言"到底查了几次"——
// 缓存有没有生效只能这么验。
func fakeGitHub(t *testing.T, status int, body string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			t.Errorf("请求路径不对: %s", r.URL.Path)
		}
		// GitHub 不带 UA 会直接拒，所以实现必须带上
		if r.Header.Get("User-Agent") == "" {
			t.Error("请求没带 User-Agent")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newTestChecker(srv *httptest.Server) *updateChecker {
	c := newUpdateChecker(SourceGitHub, "owner/repo")
	c.apiBase = srv.URL // 只为了测试能指向假服务端
	return c
}

// newTestDockerChecker 同上，但指向 Docker Hub 那个源。
func newTestDockerChecker(srv *httptest.Server) *updateChecker {
	c := newUpdateChecker(SourceDockerHub, "owner/quickshare")
	c.apiBase = srv.URL
	return c
}

// waitForCheck 等后台那次查询跑完。判据是 checking 变回 false。
func waitForCheck(t *testing.T, c *updateChecker) updateStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := c.status()
		if !st.Checking {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatal("等更新检查完成超时")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 没配仓库（本地构建就是这种）就什么都不做，更不许报错。
//
// Enabled 必须是 false —— 界面靠它区分"没配置"和"配置了但读不到"，
// 少了这个字段就只能把两种情况说成同一句话，而那句话必然有一半是错的。
func TestUpdateNoRepoMeansNoCheck(t *testing.T) {
	c := newUpdateChecker(SourceGitHub, "")
	st := c.status()
	if st.Enabled {
		t.Error("没配仓库时 Enabled 必须是 false")
	}
	if st.Checking || st.Latest != "" || st.Error != "" {
		t.Fatalf("没配仓库时不该有任何检查动作: %+v", st)
	}
	if st.Current != version.Version {
		t.Errorf("Current = %q，期望 %q", st.Current, version.Version)
	}
}

// 配了仓库就要如实说 Enabled=true。这条是"私有仓库读不到"能说真话的前提：
// 仓库私有、未认证 API 回 404 时，Latest 同样是空的，只有 Enabled 能把它们分开。
func TestUpdateEnabledReflectsRepo(t *testing.T) {
	srv, _ := fakeGitHub(t, http.StatusNotFound, `{"message":"Not Found"}`)

	st := newTestChecker(srv).checkNow()
	if !st.Enabled {
		t.Error("配了仓库时 Enabled 必须是 true")
	}
	if st.Latest != "" {
		t.Errorf("404 时不该有版本信息，实际 latest=%q", st.Latest)
	}
	if st.Error != "" {
		t.Errorf("404 不该报错，实际 %q", st.Error)
	}
}

// 第一次 status() 必须**立刻**返回，查询在后台做。
//
// 用"卡住的假服务端"来证明它确实没等：要是实现改回同步查，
// 这个用例会超时失败。这条断言是设置面板不卡的前提。
func TestUpdateStatusDoesNotBlock(t *testing.T) {
	setVersion(t, "1.0.0")

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // 卡住，直到测试放行
		_, _ = w.Write([]byte(releaseJSON))
	}))
	defer srv.Close()
	defer close(release)

	c := newTestChecker(srv)

	done := make(chan updateStatus, 1)
	go func() { done <- c.status() }()

	select {
	case st := <-done:
		if !st.Checking {
			t.Errorf("第一次查询应当回 checking=true，实际 %+v", st)
		}
		if st.Latest != "" {
			t.Errorf("第一次查询不该有结果，实际 latest=%q", st.Latest)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("status() 阻塞了——它必须立刻返回，查询在后台做")
	}
}

func TestUpdateReportsNewerRelease(t *testing.T) {
	setVersion(t, "1.0.0")
	srv, _ := fakeGitHub(t, http.StatusOK, releaseJSON)

	st := newTestChecker(srv).checkNow()
	if st.Latest != "2.0.0" {
		t.Fatalf("Latest = %q，期望 2.0.0（v 前缀要剥掉）", st.Latest)
	}
	if !st.HasUpdate {
		t.Error("1.0.0 < 2.0.0，应当提示有更新")
	}
	if st.URL != "https://example.com/rel" {
		t.Errorf("URL = %q", st.URL)
	}
	if st.CheckedAt == "" {
		t.Error("CheckedAt 不该为空")
	}
	if st.Error != "" {
		t.Errorf("不该有错误: %q", st.Error)
	}
}

func TestUpdateSameOrOlderVersion(t *testing.T) {
	for _, cur := range []string{"2.0.0", "3.0.0", "2.1.0"} {
		setVersion(t, cur)
		srv, _ := fakeGitHub(t, http.StatusOK, releaseJSON)
		if st := newTestChecker(srv).checkNow(); st.HasUpdate {
			t.Errorf("当前 %s、最新 2.0.0，不该提示有更新", cur)
		}
	}
}

// 开发版比不出大小，所以**永远不提示**有更新；但 latest 还是要带回来，
// 界面上至少能看到"最新是 2.0.0"。
func TestUpdateDevBuildNeverReportsUpdate(t *testing.T) {
	setVersion(t, version.DevVersion)
	srv, _ := fakeGitHub(t, http.StatusOK, releaseJSON)

	st := newTestChecker(srv).checkNow()
	if st.HasUpdate {
		t.Error("开发版不该提示有更新（比不出来时应当沉默）")
	}
	if st.Latest != "2.0.0" {
		t.Errorf("Latest = %q，期望 2.0.0", st.Latest)
	}
}

// 仓库还没发布过任何 release 时 GitHub 回 404。这**不是故障**，
// 不该在界面上显示成错误。
func TestUpdateNotFoundIsNotAnError(t *testing.T) {
	setVersion(t, "1.0.0")
	srv, _ := fakeGitHub(t, http.StatusNotFound, `{"message":"Not Found"}`)

	st := newTestChecker(srv).checkNow()
	if st.Error != "" {
		t.Errorf("404 不该报错，实际 %q", st.Error)
	}
	if st.Latest != "" || st.HasUpdate {
		t.Errorf("404 时不该有版本信息: %+v", st)
	}
}

func TestUpdateServerErrorIsReported(t *testing.T) {
	setVersion(t, "1.0.0")
	srv, _ := fakeGitHub(t, http.StatusInternalServerError, `{}`)

	st := newTestChecker(srv).checkNow()
	if !strings.Contains(st.Error, "500") {
		t.Errorf("Error = %q，期望提到 500", st.Error)
	}
}

// 拿到过成功结果之后，一次失败**不该把"有新版本"的提示抹掉**——
// NAS 上的网络抖一下很常见，提示跟着闪没会很莫名其妙。
func TestUpdateKeepsLastGoodResultOnError(t *testing.T) {
	setVersion(t, "1.0.0")

	var broken atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(releaseJSON))
	}))
	defer srv.Close()

	c := newTestChecker(srv)
	if st := c.checkNow(); st.Latest != "2.0.0" {
		t.Fatalf("第一次查询应当成功，实际 %+v", st)
	}

	broken.Store(true)
	st := c.checkNow()
	if st.Error == "" {
		t.Error("失败应当被记下来")
	}
	if st.Latest != "2.0.0" || !st.HasUpdate {
		t.Errorf("失败不该抹掉上一次的结果，实际 %+v", st)
	}
}

// TTL 之内反复问都不该真的去查——GitHub 未认证的限额只有 60 次/小时。
func TestUpdateCachesResult(t *testing.T) {
	setVersion(t, "1.0.0")
	srv, hits := fakeGitHub(t, http.StatusOK, releaseJSON)
	c := newTestChecker(srv)

	if st := waitForCheck(t, c); st.Latest != "2.0.0" {
		t.Fatalf("后台查询应当拿到结果，实际 %+v", st)
	}

	for i := 0; i < 5; i++ {
		c.status()
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("TTL 之内不该重复查询，实际查了 %d 次", n)
	}
}

// 「检查更新」按钮必须能绕过缓存，否则按下去什么也不会发生。
func TestUpdateCheckNowRefetches(t *testing.T) {
	setVersion(t, "1.0.0")
	srv, hits := fakeGitHub(t, http.StatusOK, releaseJSON)
	c := newTestChecker(srv)

	waitForCheck(t, c)
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("准备阶段应当只查了一次，实际 %d 次", n)
	}

	c.checkNow()
	if n := atomic.LoadInt32(hits); n != 2 {
		t.Errorf("checkNow 应当强制再查一次，实际共查了 %d 次", n)
	}
}

// ---------------------------------------------------------------- Docker Hub

// dockerTagsJSON 造一个 Docker Hub 的 tags 响应。
//
// 字段照着真实响应抄：每个 tag 都带一个 images[] 数组（三架构就是三条）加一个
// digest。**这个体积是有意义的**——它正是 Docker Hub 的响应比 GitHub 的
// release JSON 大一个数量级的原因，也是读取限额必须单独设的原因。
func dockerTagsJSON(names ...string) string {
	var b strings.Builder
	b.WriteString(`{"count":`)
	b.WriteString(strconv.Itoa(len(names)))
	b.WriteString(`,"results":[`)
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"creator":9921361,"id":1312106969,"name":`)
		b.WriteString(strconv.Quote(n))
		b.WriteString(`,"last_updated":"2026-09-16T13:33:04.21307Z","full_size":10342540,` +
			`"tag_status":"active",` +
			`"digest":"sha256:a675c3ddb967f75b622a4a71154b07243a79bf0d15a80e94dcda3cd4eeef15a1",` +
			`"images":[`)
		for j, arch := range []string{"amd64", "arm64", "arm"} {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"architecture":` + strconv.Quote(arch) +
				`,"os":"linux","variant":null,"features":"","size":10342540,"status":"active",` +
				`"digest":"sha256:e09ab6deddb8bf6826abc3b93a81a8064ba4f8f9858a70d53204cf68a6b5e699",` +
				`"last_pushed":"2026-09-16T13:32:59.252289931Z"}`)
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

// fakeDockerHub 起一个假的 Docker Hub API。同时验证路径对得上、以及带上了 UA。
func fakeDockerHub(t *testing.T, status int, body string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if !strings.HasSuffix(r.URL.Path, "/tags") {
			t.Errorf("请求路径不对: %s", r.URL.Path)
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("请求没带 User-Agent")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// Docker Hub 没有"直接给最新版"的接口，只给 tag 列表，得自己算最大。
//
// 这里的顺序**就是实际返回的顺序**：`latest` 排在最前面（它最后被推上去）。
// 所以这条用例真正防的是"图省事取 results[0]"——那会永远认为最新版叫 latest。
func TestUpdateDockerHubPicksHighestVersion(t *testing.T) {
	setVersion(t, "1.0.0")
	srv, _ := fakeDockerHub(t, http.StatusOK, dockerTagsJSON("latest", "1.0", "1.0.0"))

	st := newTestDockerChecker(srv).checkNow()
	if st.Latest != "1.0.0" {
		t.Fatalf("Latest = %q，期望 1.0.0（latest 和 1.0 都不该被选中）", st.Latest)
	}
	if !st.Enabled {
		t.Error("配了仓库时 Enabled 必须是 true")
	}
	if st.URL != "https://hub.docker.com/r/owner/quickshare/tags" {
		t.Errorf("URL = %q", st.URL)
	}
	if st.Error != "" {
		t.Errorf("不该有错误: %q", st.Error)
	}
}

// 日期式标签必须被无视：`20260916` 会被解析成 major=20260916，比任何正常版本号
// 都大。只认三段式就是为了挡这个。
func TestUpdateDockerHubIgnoresDateLikeTags(t *testing.T) {
	setVersion(t, "1.0.0")
	srv, _ := fakeDockerHub(t, http.StatusOK, dockerTagsJSON("latest", "20260916", "1.0.0", "nightly"))

	if st := newTestDockerChecker(srv).checkNow(); st.Latest != "1.0.0" {
		t.Errorf("Latest = %q，期望 1.0.0（日期式标签必须被无视）", st.Latest)
	}
}

func TestUpdateDockerHubReportsNewer(t *testing.T) {
	setVersion(t, "1.0.0")
	srv, _ := fakeDockerHub(t, http.StatusOK, dockerTagsJSON("latest", "1.0.0", "1.2.0"))

	st := newTestDockerChecker(srv).checkNow()
	if st.Latest != "1.2.0" {
		t.Fatalf("Latest = %q，期望 1.2.0", st.Latest)
	}
	if !st.HasUpdate {
		t.Error("1.0.0 < 1.2.0，应当提示有更新")
	}
}

// 私有仓库、或者仓库不存在时 Docker Hub 回 404。和 GitHub 那边一样不算故障，
// **但 Enabled 必须还是 true** —— 界面靠它说出"读不到"而不是"没配置"。
func TestUpdateDockerHubNotFoundIsNotAnError(t *testing.T) {
	setVersion(t, "1.0.0")
	srv, _ := fakeDockerHub(t, http.StatusNotFound, `{"message":"object not found"}`)

	st := newTestDockerChecker(srv).checkNow()
	if st.Error != "" {
		t.Errorf("404 不该报错，实际 %q", st.Error)
	}
	if st.Latest != "" || st.HasUpdate {
		t.Errorf("404 时不该有版本信息: %+v", st)
	}
	if !st.Enabled {
		t.Error("404 不代表没配置——Enabled 必须还是 true")
	}
}

// 一个三段式 tag 都没有（比如仓库里只推过 latest）时沉默：不报错，也不乱猜。
func TestUpdateDockerHubNoUsableTag(t *testing.T) {
	setVersion(t, "1.0.0")
	srv, _ := fakeDockerHub(t, http.StatusOK, dockerTagsJSON("latest"))

	st := newTestDockerChecker(srv).checkNow()
	if st.Error != "" {
		t.Errorf("不该报错，实际 %q", st.Error)
	}
	if st.Latest != "" {
		t.Errorf("Latest = %q，期望空（挑不出可比的三段式版本）", st.Latest)
	}
}

func TestUpdateDockerHubServerErrorIsReported(t *testing.T) {
	setVersion(t, "1.0.0")
	srv, _ := fakeDockerHub(t, http.StatusInternalServerError, `{}`)

	if st := newTestDockerChecker(srv).checkNow(); !strings.Contains(st.Error, "500") {
		t.Errorf("Error = %q，期望提到 500", st.Error)
	}
}

// Docker Hub 的响应比 GitHub 的大一个数量级，所以读取限额是单独设的。
// 这条用例塞进 150 个 tag，防止有人把它改回 GitHub 那个 64KB——那样会**正好卡在
// 中间**，把 JSON 截断成"解析失败"，而且只在 tag 多的仓库上才暴露。
func TestUpdateDockerHubHandlesLargeResponse(t *testing.T) {
	setVersion(t, "1.0.0")

	names := make([]string, 0, 160)
	names = append(names, "latest")
	for i := 0; i < 150; i++ {
		names = append(names, fmt.Sprintf("0.%d.0", i))
	}
	// 最大的放在**最后**，确保实现真的扫完了整个列表，而不是只看开头几个。
	names = append(names, "2.0.0")

	body := dockerTagsJSON(names...)
	if len(body) < updateMaxBody {
		t.Fatalf("这条用例的前提是响应体超过 GitHub 的 %d 字节限额，实际只有 %d 字节",
			updateMaxBody, len(body))
	}

	srv, _ := fakeDockerHub(t, http.StatusOK, body)
	st := newTestDockerChecker(srv).checkNow()
	if st.Error != "" {
		t.Fatalf("不该报错: %q（响应体 %d 字节）", st.Error, len(body))
	}
	if st.Latest != "2.0.0" {
		t.Errorf("Latest = %q，期望 2.0.0（响应体 %d 字节，可能被限额截断了）", st.Latest, len(body))
	}
}

// ---------------------------------------------------------------- 接口

// /api/version 是公开的：没设口令的部署同样该看到"有新版本"。
func TestVersionEndpointIsPublic(t *testing.T) {
	setVersion(t, "1.0.0")
	s := newTextTestServer(t)

	rec := doJSON(t, s.Handler(), http.MethodGet, "/api/version", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var st updateStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("解析失败: %v（原文 %s）", err, rec.Body.String())
	}
	if st.Current != "1.0.0" {
		t.Errorf("Current = %q，期望 1.0.0", st.Current)
	}
}

// 但强制刷新要口令：它会让服务端去访问外网，公开的话局域网里谁都能拿它烧配额。
func TestVersionCheckRequiresAdmin(t *testing.T) {
	setVersion(t, "1.0.0")
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(Config{DataDir: t.TempDir(), AdminToken: "secret"}, st, textTestWeb())
	h := s.Handler()

	if rec := doJSON(t, h, http.MethodPost, "/api/version/check", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("不带口令时状态码 = %d，期望 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/version/check", nil)
	req.Header.Set("X-Admin-Token", "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("带口令时状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
}

// /api/config 里的 version 必须来自构建注入，不能再写死一个假版本号。
func TestConfigReportsInjectedVersion(t *testing.T) {
	setVersion(t, "9.9.9")
	s := newTextTestServer(t)

	rec := doJSON(t, s.Handler(), http.MethodGet, "/api/config", "")
	var cfg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := cfg["version"]; got != "9.9.9" {
		t.Errorf("version = %v，期望 9.9.9", got)
	}
}
