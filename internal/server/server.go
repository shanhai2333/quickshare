// Package server 实现 QuickShare 的 HTTP 接口与页面服务。
//
// 两个页面：首页（上传 + 文件列表 + 下载），文本页（发一段文本给别的设备看）。
// 没有分享链接、提取码、有效期这些概念。
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quickshare/internal/store"
	"quickshare/internal/version"
)

// Config 是服务运行参数。
type Config struct {
	DataDir     string        // 数据根目录（文件本体 + 数据库 + 分片）
	AdminToken  string        // 管理口令；为空表示内网免鉴权
	ChunkSize   int64         // 分片大小
	MaxFileSize int64         // 单文件大小上限，0 表示不限
	UploadTTL   time.Duration // 未完成上传的保留时长

	// Version 是当前版本号，构建时用 -ldflags 注入（见 internal/version）。
	// 留空表示用 internal/version 里的默认值（本地 go build 就是 "dev"）。
	Version string

	// UpdateSource 是更新检查问哪个源：SourceGitHub（默认）或 SourceDockerHub。
	UpdateSource string

	// UpdateRepo 是 "owner/name" 形式的仓库名，**属于 UpdateSource 那个源**，
	// 更新检查用。**留空表示不做更新检查** —— 本地构建、以及显式关掉检查的
	// 部署都是这种。
	//
	// 注意两个源的命名空间是分开的：GitHub 上叫 shanhai2333/quickshare、
	// Docker Hub 上可能叫 shanhaijun/quickshare，不能混用。
	UpdateRepo string

	// DataDirLocked 为真表示数据目录由部署方式决定（命令行参数或环境变量），
	// 设置界面里只读。docker/compose 用卷映射的场景就是这种。
	DataDirLocked bool
	// PersistDataDir 把新的数据目录写进启动配置，供下次启动读取。
	// 为 nil 表示没有可写配置（比如只读部署），此时不允许在界面里改目录。
	PersistDataDir func(dir string) error

	// TrustedProxies 是受信代理名单（`QS_TRUSTED_PROXIES`），逗号分隔的 IP 或网段。
	//
	// **留空表示一个代理都不信**，也就是完全不去看 X-Forwarded-For —— 这是默认值，
	// 也是唯一安全的值：那个头是请求方随便写的，无条件采信等于把设备身份交回给请求方。
	// 只有把反向代理自己的地址配进来，服务才会去读它，且只认"从右往左第一个
	// 不受信的地址"。详见 proxy.go。
	TrustedProxies string
}

// backend 是"当前正在用的数据目录 + 数据库句柄"。
//
// 这两者必须一起换：切换存储目录时，如果数据库换了而目录没换（或反过来），
// 就会出现"记录指向旧目录的文件"这种错位。打包成一个不可变结构、
// 用原子指针整体替换，读侧无锁，也就不会读到半新半旧的状态。
type backend struct {
	st      *store.Store
	dataDir string
}

func (b *backend) filePath(id string) string {
	return filepath.Join(b.dataDir, "files", id)
}

func (b *backend) chunkDir(id string) string {
	return filepath.Join(b.dataDir, "chunks", id)
}

func (b *backend) chunkPath(id string, idx int) string {
	return filepath.Join(b.chunkDir(id), strconv.Itoa(idx))
}

func (b *backend) bgPath() string {
	return filepath.Join(b.dataDir, "background")
}

// Server 持有依赖并暴露 HTTP handler。
type Server struct {
	cfg    Config
	web    fs.FS
	assets string // 前端资源版本号，用于 URL 破缓存

	cur       atomic.Pointer[backend] // 当前数据目录与数据库
	switchMu  sync.Mutex              // 串行化存储目录切换
	switching atomic.Bool             // 切换期间拒绝请求，避免读到已关闭的数据库

	// 文件列表变更的广播中心。挂在 Server 上而不是 backend 上：
	// 切换数据目录时连接不该被掐断——前端要的正是"切完目录后列表跟着变"。
	events *eventHub

	// 版本更新检查（问 GitHub 有没有新 release），自带缓存。
	update *updateChecker

	// 解析好的受信代理名单（`QS_TRUSTED_PROXIES`）。**为空表示一个都不信**，
	// 也就是不去看 X-Forwarded-For，行为跟以前完全一样。
	proxies trustedProxies
}

// New 构造 Server。web 是内嵌的前端静态资源根目录。
func New(cfg Config, st *store.Store, web fs.FS) *Server {
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = 8 << 20 // 8 MiB
	}
	if cfg.UploadTTL <= 0 {
		cfg.UploadTTL = 24 * time.Hour
	}
	if cfg.Version == "" {
		cfg.Version = version.Version
	}
	s := &Server{
		cfg:     cfg,
		web:     web,
		assets:  assetVersion(web),
		events:  newEventHub(),
		update:  newUpdateChecker(cfg.UpdateSource, cfg.UpdateRepo),
		proxies: parseTrustedProxies(cfg.TrustedProxies),
	}
	if len(s.proxies) > 0 {
		// 配了就打一行，让"为什么设备列表里现在是真实 IP"这件事有据可查。
		// 这是个安全相关的开关，静默生效不合适。
		log.Printf("  受信代理   %s（只有来自这些地址的 X-Forwarded-For 才被采信）",
			cfg.TrustedProxies)
	}
	s.cur.Store(&backend{st: st, dataDir: cfg.DataDir})
	return s
}

// be 取当前后端。所有需要访问数据库或数据目录的地方都从这里拿，
// 不要直接摸 s.cfg.DataDir——那个字段在切换后就是旧值了。
func (s *Server) be() *backend { return s.cur.Load() }

// assetVersion 把内嵌的前端资源算成一个短哈希当版本号。
//
// 内容一变版本号就变，页面引用的 URL 跟着变，浏览器自然拿不到旧资源。
// 靠"记得手动改版本号"迟早会忘，这里直接从内容算，忘不了。
func assetVersion(web fs.FS) string {
	h := sha256.New()
	// WalkDir 是字典序遍历，同一份资源每次算出来都一样
	_ = fs.WalkDir(web, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		_, _ = io.WriteString(h, p)
		f, err := web.Open(p)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()
		_, _ = io.Copy(h, f)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// ---------------------------------------------------------------- 路由

// Handler 返回挂载了全部路由的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 公开接口
	mux.HandleFunc("GET /api/config", s.handleConfig)
	// 版本与更新检查。**读取是公开的**：版本号不敏感，而且没设口令的部署
	// 同样该看到"有新版本"。强制刷新要口令——它会让服务端去访问外网，
	// 公开的话局域网里谁都能拿它烧掉 GitHub 的接口配额。
	mux.HandleFunc("GET /api/version", s.handleVersion)
	mux.HandleFunc("POST /api/version/check", s.admin(s.handleVersionCheck))
	// 二维码：内容完全由调用方通过 ?d= 传入，服务端不读自己的任何数据，
	// 也就不存在"泄露了什么"的问题，所以不需要口令。
	mux.HandleFunc("GET /api/qr", s.handleQR)
	mux.HandleFunc("GET /f/{id}", s.handleDownload)
	mux.HandleFunc("GET /f/{id}/{name}", s.handleDownload)

	// 设置（外观 + 文本保留时间）：读取公开（页面要在鉴权前就套上主题，
	// 文本页首帧也要拿保留时长显示提示），写入需管理权限
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.admin(s.handlePutSettings))
	mux.HandleFunc("GET /api/background", s.handleGetBackground)
	mux.HandleFunc("POST /api/background", s.admin(s.handleUploadBackground))
	mux.HandleFunc("DELETE /api/background", s.admin(s.handleDeleteBackground))

	// 文件存储位置。路径属于部署信息，不该让未鉴权的访问者看到，所以读写都要口令。
	mux.HandleFunc("GET /api/storage", s.admin(s.handleGetStorage))
	mux.HandleFunc("PUT /api/storage", s.admin(s.handlePutStorage))

	// 上传接口
	mux.HandleFunc("POST /api/upload/init", s.admin(s.handleUploadInit))
	mux.HandleFunc("PUT /api/upload/{id}/{idx}", s.admin(s.handleUploadChunk))
	mux.HandleFunc("GET /api/upload/{id}/status", s.admin(s.handleUploadStatus))
	mux.HandleFunc("POST /api/upload/{id}/complete", s.admin(s.handleUploadComplete))
	mux.HandleFunc("DELETE /api/upload/{id}", s.admin(s.handleUploadCancel))

	// 列表与删除
	mux.HandleFunc("GET /api/stats", s.admin(s.handleStats))
	mux.HandleFunc("GET /api/files", s.admin(s.handleListFiles))
	mux.HandleFunc("DELETE /api/files/{id}", s.admin(s.handleDeleteFile))
	// 改单个文件的保留时长。跟文本那边一样，只动这一条、不动全局设置。
	mux.HandleFunc("PUT /api/files/{id}", s.admin(s.handleSetFileTTL))

	// 共享文本（便签）。跟文件列表同级，同样走管理权限包装。
	mux.HandleFunc("GET /api/texts", s.admin(s.handleListTexts))
	mux.HandleFunc("POST /api/texts", s.admin(s.handleCreateText))
	// 批量删除。放在 {id} 之前无所谓（路径段数不同，不冲突），
	// 但用 POST 而不是带 body 的 DELETE：后者在反代/缓存那里处理不一致。
	mux.HandleFunc("POST /api/texts/delete", s.admin(s.handleDeleteTexts))
	mux.HandleFunc("PUT /api/texts/{id}", s.admin(s.handleUpdateText))
	mux.HandleFunc("DELETE /api/texts/{id}", s.admin(s.handleDeleteText))
	// 设备：改备注用。设备本身是文本页的副产品，没有单独的增删接口。
	mux.HandleFunc("GET /api/devices", s.admin(s.handleListDevices))
	mux.HandleFunc("PUT /api/devices/{id}", s.admin(s.handleSetDeviceRemark))
	mux.HandleFunc("DELETE /api/devices/{id}", s.admin(s.handleDeleteDevice))

	// 列表变更推送（SSE 长连接）。需要口令：它暴露的是"这个实例正在被使用"
	// 这类活动信息，跟列表本身同级。
	mux.HandleFunc("GET /api/events", s.admin(s.handleEvents))

	// 静态资源。两个页面单独接管：都要把服务端存的主题写进 HTML，保证首帧配色正确。
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /text", s.handleTextPage)
	mux.Handle("GET /", s.static())

	return s.securityHeaders(s.logRequests(s.guardSwitch(mux)))
}

// guardSwitch 在切换存储目录期间挡掉请求。
//
// 切换时要先关掉旧数据库才能把数据搬走，这中间有个窗口：如果放请求进去，
// 它们会撞上一个已关闭的 *sql.DB，返回一堆"database is closed"。
// 与其让用户看到莫名其妙的 500，不如明确回 503 让他重试。
func (s *Server) guardSwitch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.switching.Load() {
			w.Header().Set("Retry-After", "2")
			writeErr(w, http.StatusServiceUnavailable, "正在切换存储目录，请稍候重试")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// static 提供内嵌的前端资源。
//
// 页面引用资源时会带上 ?v=<内容哈希>，这种 URL 的内容永不改变，可以放心
// 长缓存；没带版本号的（比如手动敲进来的地址）一律要求重新校验。
func (s *Server) static() http.Handler {
	files := http.FileServer(http.FS(s.web))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// StartCleanup 启动后台清理任务：删除残留的未完成上传与过期的共享文本。
//
// 两种清理共用一个 ticker：它们都是"顺手扫一下"的低频任务，
// 分成两个 goroutine 只是多一份调度，没有实际收益。
//
// 返回的停止函数**会等 goroutine 真的退出**再返回。不这样做的话，调用方紧接着
// 关数据库（main 里就是 stopCleanup 先跑、st.Close 后跑），而清理任务可能正卡在
// 一次 runCleanup 中间——于是每次关服务都稳定吐一行
// "清理过期文本失败: sql: database is closed"。不致命，但会让关停日志变得不可信，
// 真出问题时容易被当成噪音划过去。
func (s *Server) StartCleanup(interval time.Duration) func() {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			s.runCleanup()
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()

	// once 兜住重复调用：close(stop) 关第二次会 panic，而"停两次"是很自然的误用
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-done // 等它真的退出，否则会和随后的关库抢
	}
}

func (s *Server) runCleanup() {
	if s.switching.Load() {
		return // 正在换目录，这一轮跳过，别和搬迁抢文件
	}
	b := s.be()
	ids, err := b.st.StaleUploads(s.cfg.UploadTTL)
	if err != nil {
		log.Printf("查询残留上传失败: %v", err)
	} else {
		for _, id := range ids {
			if err := b.st.DeleteFile(id); err != nil {
				continue
			}
			_ = os.RemoveAll(b.chunkDir(id))
			log.Printf("已清理未完成上传 %s", id)
		}
	}
	s.purgeTexts(b)
	s.purgeFiles(b)
}

// purgeTexts 按保留规则清理文本。三档都没启用就什么都不做。
//
// 定时器是 10 分钟一轮，所以实际删除时刻会比设定值晚最多 10 分钟。
// 对"留 1 小时"这种场景是可以接受的误差，不值得为它把轮询调密——
// 那会让空闲实例每几分钟就查一次库。
//
// 注意验证码那档默认是**开着**的（10 分钟），所以升级上来之后，"长得像验证码"
// 的文本会开始被自动清掉。这是设计如此，不是 bug。
func (s *Server) purgeTexts(b *backend) {
	kv, err := b.st.GetSettings()
	if err != nil {
		// 读设置失败不能 return 掉整个清理——上传清理在它前面跑，
		// 但文件清理在它后面。这里只跳过文本这一段。
		log.Printf("读取文本保留设置失败: %v", err)
		return
	}
	p := policyFrom(kv)
	if p.text <= 0 && p.code <= 0 {
		return // 两档都没启用 = 永不自动删除
	}

	n, err := b.st.PurgeTexts(time.Now(), p.textSeconds(), p.codeSeconds())
	if err != nil {
		log.Printf("清理过期文本失败: %v", err)
		return
	}
	if n > 0 {
		log.Printf("已清理 %d 条过期文本", n)
		// 让打开的页面跟着把这几条抹掉，不然它们会一直挂在界面上，
		// 用户点进去才发现已经没了
		s.events.notify(topicTexts)
		// 自动清理同样算"消息没了"，开着「设备随消息删除」时设备记录要一起走。
		// 不然过一阵子回来一看：文本早清光了，设备列表里还挂着一堆空设备，
		// 而用户并没有手动删过任何东西——最莫名其妙的正是这种。
		s.pruneDevicesIfEnabled()
	}
}

// purgeFiles 按保留规则清理文件。
//
// 跟文本那边一样是"单条 > 全局"两档（文件没有验证码那一档）。
// 库里的记录和磁盘上的本体必须一起删：只删库会留下永远没人认领的垃圾文件，
// 只删文件则列表里还挂着一个点开就 404 的条目。
func (s *Server) purgeFiles(b *backend) {
	kv, err := b.st.GetSettings()
	if err != nil {
		log.Printf("读取文件保留设置失败: %v", err)
		return
	}
	p := policyFrom(kv)
	// 这里**不**因为全局那档没启用就提前返回：某几条文件可能自己设了保留时长，
	// 全局为 0 时照样得按它们各自的时长清。传 0 进去，store 会把全局那档
	// 折算成"永不命中"。
	ids, err := b.st.ExpiredFiles(time.Now(), p.fileSeconds())
	if err != nil {
		log.Printf("查询过期文件失败: %v", err)
		return
	}
	if len(ids) == 0 {
		return
	}

	gone := 0
	for _, id := range ids {
		if err := b.st.DeleteFile(id); err != nil {
			continue
		}
		_ = os.Remove(b.filePath(id))
		_ = os.RemoveAll(b.chunkDir(id))
		gone++
	}
	if gone > 0 {
		log.Printf("已清理 %d 个过期文件", gone)
		s.events.notify(topicFiles)
	}
}

// ---------------------------------------------------------------- 中间件

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		// 状态轮询与背景图噪音较大，不做记录
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/background" {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")

		// 应用页面自身的 CSP：只允许同源资源，禁止被嵌进 iframe，禁止表单外发。
		//
		// 注意 CSP 不拦 CSSOM（`element.style.x = ...`），所以进度条宽度、背景图
		// 这些由 JS 设置的内联样式照常生效；但 HTML 里写死的 style="" 会被拦，
		// 因此 index.html 里那几处已经抽成 class 了。
		//
		// /f/ 下的上传文件不走这条：它们的策略由 handleDownload 按类型决定。
		// 给 PDF、视频套 default-src 'self' 有可能影响浏览器内置查看器，没必要冒险。
		if !strings.HasPrefix(r.URL.Path, "/f/") {
			w.Header().Set("Content-Security-Policy",
				"default-src 'self'; img-src 'self' data:; style-src 'self'; "+
					"script-src 'self'; object-src 'none'; base-uri 'none'; "+
					"frame-ancestors 'none'; form-action 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

// admin 包一层管理权限校验。未配置口令时（内网自用）直接放行。
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken != "" && !s.adminOK(r) {
			writeErr(w, http.StatusUnauthorized, "管理口令不正确")
			return
		}
		next(w, r)
	}
}

func (s *Server) adminOK(r *http.Request) bool {
	got := r.Header.Get("X-Admin-Token")
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	if got == "" {
		if c, err := r.Cookie("qs_admin"); err == nil {
			got = c.Value
		}
	}
	return subtleEqual(got, s.cfg.AdminToken)
}

// ---------------------------------------------------------------- 接口

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"needAuth":    s.cfg.AdminToken != "",
		"chunkSize":   s.cfg.ChunkSize,
		"maxFileSize": s.cfg.MaxFileSize,
		// 构建时注入的版本号。以前这里写死 "1.0.0"，跟实际编出来的东西
		// 一点关系都没有——用户报问题时报的版本号是假的，最坑。
		"version": s.cfg.Version,
		// 回显请求方自己的 IP。设备身份就是它，让页面随时能显示"你是哪台"，
		// 不必等到发过文本、在设备表里有了记录才知道。回显自己的地址，
		// 不涉及任何别人的信息，所以这个公开接口可以给。
		"clientIp": s.clientIP(r),
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	files, size, err := s.be().st.Stats()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "totalSize": size})
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	files, err := s.be().st.ListFiles()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 保留规则整份读一次，逐条复用（跟文本列表同一个做法）
	p, err := s.loadTTLPolicy()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type dto struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Size      int64  `json:"size"`
		Mime      string `json:"mime"`
		CreatedAt int64  `json:"createdAt"`
		URL       string `json:"url"`
		// Preview 是前端预览层该用哪种渲染方式（image / video / audio / pdf / text），
		// 空串表示这个类型不给预览、只能下载。
		//
		// **由服务端给，不是前端自己按 mime 前缀猜**：能不能内联渲染是
		// previewModeOf 那份白名单说了算的，前端再维护一份就会漂——漂了的表现是
		// "点了预览直接触发下载"。
		Preview string `json:"preview"`
		// TTLSeconds 是这条自己的保留时长，0 = 跟随全局。前端拿它回填编辑框。
		TTLSeconds int64 `json:"ttlSeconds"`
		// ExpiresAt 是到期时刻（unix 秒），**0 表示永不删除**。
		ExpiresAt int64 `json:"expiresAt"`
	}
	out := make([]dto, 0, len(files))
	for _, f := range files {
		out = append(out, dto{
			ID: f.ID, Name: f.Name, Size: f.Size, Mime: f.Mime,
			CreatedAt:  f.CreatedAt.Unix(),
			URL:        "/f/" + f.ID + "/" + urlEscape(f.Name),
			Preview:    previewKindOf(f.Mime),
			TTLSeconds: f.TTLSeconds,
			ExpiresAt:  p.fileExpiry(f),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSetFileTTL 改一个文件自己的保留时长。
//
// 单独一个接口而不是塞进 PUT /api/files/{id}：文件那侧目前只有"删"这一个写操作，
// 为它造一个只认一个字段的 PUT 不如直接给个语义明确的路径。
// ttlSeconds 为 0 表示改回"跟随全局设置"。
func (s *Server) handleSetFileTTL(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TTLSeconds *int64 `json:"ttlSeconds"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	if in.TTLSeconds == nil {
		writeErr(w, http.StatusBadRequest, "缺少 ttlSeconds")
		return
	}
	if v := *in.TTLSeconds; v < 0 || v > maxItemTTLSeconds {
		writeErr(w, http.StatusBadRequest,
			"保留时长取值范围是 0 到 "+strconv.FormatInt(maxItemTTLSeconds, 10)+
				" 秒（0 表示跟随全局设置）")
		return
	}

	b := s.be()
	id := r.PathValue("id")
	if err := b.st.SetFileTTL(id, *in.TTLSeconds); err != nil {
		writeStoreErr(w, err, "文件不存在")
		return
	}

	f, err := b.st.GetFile(id)
	if err != nil {
		writeStoreErr(w, err, "文件不存在")
		return
	}
	p, err := s.loadTTLPolicy()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.events.notify(topicFiles)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         f.ID,
		"ttlSeconds": f.TTLSeconds,
		"expiresAt":  p.fileExpiry(f),
	})
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	b := s.be()
	id := r.PathValue("id")
	if err := b.st.DeleteFile(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "文件不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.Remove(b.filePath(id))
	_ = os.RemoveAll(b.chunkDir(id))
	s.events.notify(topicFiles)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- 工具

// urlEscape 做 path segment 转义，保留部分可读字符。
func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hexByte(c)))
		}
	}
	return b.String()
}

const hexDigits = "0123456789ABCDEF"

func hexByte(c byte) string {
	return string([]byte{hexDigits[c>>4], hexDigits[c&0x0f]})
}

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// subtleEqual 定长比较，避免时序侧信道。
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	defer func() { _, _ = io.Copy(io.Discard, r.Body) }()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(v)
}
