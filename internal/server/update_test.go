package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	c := newUpdateChecker("owner/repo")
	c.apiBase = srv.URL // 只为了测试能指向假服务端
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
func TestUpdateNoRepoMeansNoCheck(t *testing.T) {
	c := newUpdateChecker("")
	st := c.status()
	if st.Checking || st.Latest != "" || st.Error != "" {
		t.Fatalf("没配仓库时不该有任何检查动作: %+v", st)
	}
	if st.Current != version.Version {
		t.Errorf("Current = %q，期望 %q", st.Current, version.Version)
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
