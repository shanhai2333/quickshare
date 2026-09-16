package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"quickshare/internal/version"
)

// 更新检查的几个时间常数。
const (
	// 成功结果的保鲜期。GitHub 未认证的 API 限额是每小时 60 次/IP，
	// 6 小时一次既够及时（内网自用的服务，没人指望分钟级），又留足余量。
	updateTTL = 6 * time.Hour

	// 失败后的重试间隔。比成功短（网络抖一下能较快自愈），但也不能太短——
	// 否则一台没有外网的 NAS 会变成"每来一个请求就发一次必然失败的请求"。
	updateErrTTL = 30 * time.Minute

	// 单次查询超时。它决定的是**后台那次 goroutine**挂多久，所以要短：
	// 内网 NAS 可能完全没有外网，这个值直接等于白等的时间。
	updateTimeout = 5 * time.Second

	// 响应体最多读多少字节。GitHub 的 release JSON 里 assets 列表可能很大，
	// 而我们只要四个字段。
	updateMaxBody = 64 << 10

	// Docker Hub 的 tags 列表比 GitHub 的 release JSON 大得多：**每个 tag 都带
	// 一个 images[] 数组**（三架构就是三条），一条 tag 七百字节上下，100 条就近 70KB。
	// 沿用 GitHub 那个 64KB 的限额会正好卡在中间，把 JSON 截断成"解析失败"。
	updateMaxBodyDockerHub = 1 << 20
)

// 更新检查的两个源。用字符串而不是枚举，是为了直接对上环境变量的取值。
const (
	// SourceGitHub 问 GitHub 的 releases/latest——它直接给出最新版，最省事。
	SourceGitHub = "github"

	// SourceDockerHub 问 Docker Hub 的 tag 列表。**私有 GitHub 仓库用这个**：
	// 未认证请求查私有仓库一律 404，而 Docker Hub 的公开仓库不认证也能读。
	SourceDockerHub = "dockerhub"
)

// 两个源的 API 根地址。抽成字段只为了测试能指向 httptest 服务。
const (
	githubAPIBase    = "https://api.github.com"
	dockerHubAPIBase = "https://hub.docker.com"
)

// updateStatus 是 /api/version 的响应体，也是界面直接渲染的东西。
type updateStatus struct {
	// Enabled 表示这个构建**配了**更新检查。
	//
	// 为什么非要单独给一个字段：Latest 为空有两种截然不同的原因——"压根没配"
	// 和"配了但读不到"（仓库是私有的、还没发过 Release、网络不通）。只靠 Latest
	// 为空分不出来，界面就只能挑一句话说，而那句话对其中一半情况必然是错的。
	// 这个字段就是让界面能说实话的那把钥匙。
	Enabled   bool   `json:"enabled"`
	Current   string `json:"current"`   // 当前版本（构建时注入）
	Latest    string `json:"latest"`    // 最新版本；空 = 还不知道
	HasUpdate bool   `json:"hasUpdate"` // 当前 < 最新
	URL       string `json:"url"`       // 发布页
	Published string `json:"publishedAt"`
	CheckedAt string `json:"checkedAt"` // 上次查询成功的时刻，RFC3339
	Checking  bool   `json:"checking"`  // 正在查；前端可以过一会儿再问一次
	Error     string `json:"error"`     // 查询失败的原因（给人看的）
}

// updateChecker 负责"问远端有没有新版本"，并把它缓存起来。
//
// 支持两个源（见 SourceGitHub / SourceDockerHub），差别只在 fetch 那一层。
//
// 为什么不每次请求都去查：未认证的 API 都是按 IP 限流的（GitHub 只有 60 次/小时），
// 而且内网部署的 NAS 很可能压根没有外网——每次打开设置面板都去连一次，
// 轻则烧配额，重则每次白等 5 秒。
type updateChecker struct {
	// source 是 SourceGitHub 或 SourceDockerHub，决定查哪个 API。
	source  string
	repo    string
	apiBase string
	client  *http.Client

	// fetchMu 串行化"真正去查一次"：后台刷新和按钮触发的强制刷新
	// 不会同时打两次远端。
	fetchMu sync.Mutex

	mu      sync.Mutex
	latest  string
	url     string
	pub     string
	at      time.Time // 上次**尝试**查询的时刻（成功失败都算）
	err     string
	running bool
}

// newUpdateChecker 建一个检查器。**repo 为空 = 不做检查。**
//
// source 只认 SourceGitHub / SourceDockerHub；别的值按 GitHub 处理。取值校验
// 放在 main.go（那里能往 stderr 写一句人看得懂的话），这里不重复一遍。
func newUpdateChecker(source, repo string) *updateChecker {
	c := &updateChecker{
		source: source,
		repo:   strings.TrimSpace(repo),
		client: &http.Client{Timeout: updateTimeout},
	}
	if c.source == SourceDockerHub {
		c.apiBase = dockerHubAPIBase
	} else {
		c.apiBase = githubAPIBase
	}
	return c
}

// status 返回当前状态，并在需要时**在后台**发起一次刷新。
//
// 刻意不在这里同步查：NAS 上没有外网时一次查询要挂满超时，而设置面板一打开
// 就会调这个接口——同步查等于每次打开面板都卡几秒。所以这里立刻回旧值
// （第一次是空的），前端看到 checking=true 时过几秒再问一次。
func (c *updateChecker) status() updateStatus {
	if c.repo == "" {
		return updateStatus{Current: version.Version}
	}

	c.mu.Lock()
	if c.staleLocked() && !c.running {
		// running 在锁里检查并置位，所以最多只会有一个后台 goroutine，
		// 不需要额外的去重逻辑。
		c.running = true
		go c.refresh()
	}
	c.mu.Unlock()

	return c.snapshot()
}

// checkNow 同步查一次并返回新状态（设置面板里「检查更新」按钮用）。
//
// 刻意**不**先清空已有结果：查的时候界面还显示着上一次的版本信息，
// 按钮上转个"检查中…"就够了，不必先闪成空白。
func (c *updateChecker) checkNow() updateStatus {
	if c.repo == "" {
		return updateStatus{Current: version.Version}
	}

	c.fetchMu.Lock()
	tag, url, pub, err := c.fetch()
	c.fetchMu.Unlock()

	c.store(tag, url, pub, err)
	return c.snapshot()
}

// refresh 是后台那次刷新。
func (c *updateChecker) refresh() {
	c.fetchMu.Lock()
	tag, url, pub, err := c.fetch()
	c.fetchMu.Unlock()

	c.store(tag, url, pub, err)
}

// store 落库查询结果。失败时**保留上一次的成功结果**——网络抖一下不该让
// "有新版本"的提示凭空消失，只把错误记下来供界面显示。
func (c *updateChecker) store(tag, url, pub string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.running = false
	c.at = time.Now()
	if err != nil {
		c.err = err.Error()
		return
	}
	c.err = ""
	c.latest, c.url, c.pub = tag, url, pub
}

// snapshot 读一份当前状态的快照。
func (c *updateChecker) snapshot() updateStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	st := updateStatus{
		Enabled:   c.repo != "",
		Current:   version.Version,
		Latest:    c.latest,
		URL:       c.url,
		Published: c.pub,
		Checking:  c.running,
		Error:     c.err,
	}
	if !c.at.IsZero() {
		st.CheckedAt = c.at.UTC().Format(time.RFC3339)
	}
	// 比不出来时 Compare 返回 0，也就是"不提示有更新"——这是刻意的，
	// 详见 version.Compare 的注释。
	st.HasUpdate = st.Latest != "" && version.Compare(st.Current, st.Latest) < 0
	return st
}

// staleLocked 判断缓存该不该重查。调用方必须持有 c.mu。
func (c *updateChecker) staleLocked() bool {
	if c.at.IsZero() {
		return true
	}
	ttl := updateTTL
	if c.err != "" {
		ttl = updateErrTTL
	}
	return time.Since(c.at) >= ttl
}

// fetch 按 source 去问对应的源。返回的版本号已经剥掉了 v 前缀。
func (c *updateChecker) fetch() (tag, url, published string, err error) {
	if c.source == SourceDockerHub {
		return c.fetchDockerHub()
	}
	return c.fetchGitHub()
}

// fetchGitHub 问 releases/latest——它直接给出最新版，最省事。
func (c *updateChecker) fetchGitHub() (tag, url, published string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.apiBase+"/repos/"+c.repo+"/releases/latest", nil)
	if err != nil {
		return "", "", "", err
	}
	// GitHub 要求带 UA，不带会被直接拒掉；Accept 钉住版本，
	// 免得将来默认格式变了把解析弄坏。
	req.Header.Set("User-Agent", "QuickShare/"+version.Version)
	req.Header.Set("Accept", "application/vnd.github+json")

	res, err := c.client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer func() { _ = res.Body.Close() }()

	// 404 = 仓库还没有任何 release，或者仓库名写错了。**这不算故障**，
	// 别把它当错误显示给用户："还没有发布过"是完全正常的状态。
	if res.StatusCode == http.StatusNotFound {
		return "", "", "", nil
	}
	if res.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("GitHub 返回 %d", res.StatusCode)
	}

	var body struct {
		TagName     string `json:"tag_name"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, updateMaxBody)).Decode(&body); err != nil {
		return "", "", "", fmt.Errorf("解析 GitHub 响应失败: %w", err)
	}
	// /releases/latest 本身就会跳过 draft 和 prerelease，不用自己筛。
	return strings.TrimPrefix(body.TagName, "v"), body.HTMLURL, body.PublishedAt, nil
}

// fetchDockerHub 从 tag 列表里挑出最大的三段式版本。
//
// Docker Hub 没有 GitHub 那种"直接告诉我最新版"的接口，只给一个 tag 列表，
// 所以"哪个最新"得自己算。**绝不能取 results[0]**：列表按最后推送时间倒序，
// 而 `latest` 这个标签往往就是最后推上去的，永远排在最前面——取第一个等于
// 永远认为最新版叫 "latest"。
//
// 分页：默认按最后推送时间倒序，最新的几个一定在第一页里，所以只读一页就够
// （page_size 给 100，正常项目用不完）。
func (c *updateChecker) fetchDockerHub() (tag, url, published string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.apiBase+"/v2/repositories/"+c.repo+"/tags?page_size=100", nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("User-Agent", "QuickShare/"+version.Version)
	req.Header.Set("Accept", "application/json")

	res, err := c.client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer func() { _ = res.Body.Close() }()

	// 404 = 仓库不存在、或者是私有的。**和 GitHub 那边一样不算故障**：
	// 界面靠 enabled 字段说明这到底是"没配"还是"读不到"，不靠这个错误。
	if res.StatusCode == http.StatusNotFound {
		return "", "", "", nil
	}
	if res.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("Docker Hub 返回 %d", res.StatusCode)
	}

	var body struct {
		Results []struct {
			Name        string `json:"name"`
			LastUpdated string `json:"last_updated"`
		} `json:"results"`
	}
	dec := json.NewDecoder(io.LimitReader(res.Body, updateMaxBodyDockerHub))
	if err := dec.Decode(&body); err != nil {
		return "", "", "", fmt.Errorf("解析 Docker Hub 响应失败: %w", err)
	}

	// 列表里混着 `latest`、`1.0`、日期式标签，只认三段式再取最大的那个。
	// IsFullSemver 的注释里写了为什么这么严。
	for _, r := range body.Results {
		if !version.IsFullSemver(r.Name) {
			continue
		}
		if tag == "" || version.Compare(r.Name, tag) > 0 {
			tag, published = r.Name, r.LastUpdated
		}
	}
	if tag == "" {
		// 一个三段式 tag 都没有（比如仓库里只有 `latest`）。不是错误，
		// 只是"查不到能比的版本"，和 404 一样走"沉默"。
		return "", "", "", nil
	}
	return tag, "https://hub.docker.com/r/" + c.repo + "/tags", published, nil
}

// ---------------------------------------------------------------- 接口

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.update.status())
}

func (s *Server) handleVersionCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.update.checkNow())
}
