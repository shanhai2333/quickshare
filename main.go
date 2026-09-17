// QuickShare — 一个跑在 NAS 上的内网文件分享服务。
//
// 单个二进制文件，内嵌前端与 SQLite 驱动，无需任何外部依赖。
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"quickshare/internal/desktop"
	"quickshare/internal/server"
	"quickshare/internal/store"
	"quickshare/internal/version"
)

//go:embed all:web
var webAssets embed.FS

func main() {
	log.SetFlags(log.LstdFlags)

	var (
		addr = flag.String("addr", envOr("QS_ADDR", ":8080"), "监听地址，如 :8080")
		data = flag.String("data", envOr("QS_DATA_DIR", "./data"), "数据目录（文件本体与数据库）")
		tray = flag.Bool("tray", envBool("QS_TRAY", true), "在系统托盘显示图标（仅 Windows）")
	)
	flag.Parse()

	// 数据目录的来源决定了能不能在设置界面里改它：
	// 命令行参数和环境变量是部署方明确的意图（docker 的卷映射就是这种），锁死；
	// 两者都没给才去读启动配置，配置里的值允许在界面上改。
	explicitData := os.Getenv("QS_DATA_DIR") != ""
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "data" {
			explicitData = true
		}
	})
	launch := loadLaunchConfig()
	dataDir := *data
	if !explicitData && launch.DataDir != "" {
		dataDir = launch.DataDir
	}

	// 更新检查问哪个源、查哪个仓库。两个都返回空串表示不做检查。
	updateSource, updateRepo := updateTarget()

	cfg := server.Config{
		DataDir:       dataDir,
		AdminToken:    os.Getenv("QS_ADMIN_TOKEN"),
		ChunkSize:     envInt("QS_CHUNK_SIZE", 8<<20),
		MaxFileSize:   envInt("QS_MAX_FILE_SIZE", 0),
		UploadTTL:     time.Duration(envInt("QS_UPLOAD_TTL_HOURS", 24)) * time.Hour,
		Version:       version.Version,
		UpdateSource:  updateSource,
		UpdateRepo:    updateRepo,
		DataDirLocked: explicitData,
		PersistDataDir: func(dir string) error {
			launch.DataDir = dir
			return saveLaunchConfig(launch)
		},
	}

	for _, dir := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "files"), filepath.Join(cfg.DataDir, "chunks")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			// 权限问题单独给提示：这是容器部署时最常撞的墙，而 "permission denied"
			// 本身完全没说清"是谁没权限、该改哪里"，用户只能猜。
			if errors.Is(err, fs.ErrPermission) {
				log.Fatalf("创建目录 %s 失败: %v\n%s", dir, err, permissionHint)
			}
			log.Fatalf("创建目录 %s 失败: %v", dir, err)
		}
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "quickshare.db"))
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	webFS, err := fs.Sub(webAssets, "web")
	if err != nil {
		log.Fatalf("加载内嵌前端资源失败: %v", err)
	}

	srv := server.New(cfg, st, webFS)
	stopCleanup := srv.StartCleanup(10 * time.Minute)
	defer stopCleanup()

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// 不设 WriteTimeout：大文件下载可能持续很久
		IdleTimeout: 120 * time.Second,
	}

	banner(cfg, *addr)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// 托盘图标（Windows）：右下角一个图标，右键能打开页面或退出。
	// 其它平台 Supported() 为 false，这里整段跳过。
	quit := make(chan struct{})
	localURL := localURLOf(*addr)
	if *tray && desktop.Supported() {
		err := desktop.StartTray(desktop.Options{
			Tooltip: "QuickShare · " + localURL,
			OnOpen: func() {
				if err := desktop.OpenURL(localURL); err != nil {
					log.Printf("打开页面失败: %v", err)
				}
			},
			OnQuit: func() {
				log.Println("从托盘退出")
				close(quit)
			},
		})
		if err != nil {
			log.Printf("托盘图标未启用: %v", err)
		} else {
			log.Printf("  托盘图标   已启用（右下角图标右键可退出）")
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		log.Fatalf("服务异常退出: %v", err)
	case sig := <-sigCh:
		log.Printf("收到信号 %v，正在关闭…", sig)
	case <-quit:
		// 托盘的「退出」回调里已经打过日志了
	}

	// 先把托盘图标摘掉：关机流程可能要等一会儿（大文件下载要收尾），
	// 这段时间图标还挂着、点了却没反应，不如立刻消失。
	desktop.StopTray()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("关闭超时: %v", err)
	}
	log.Println("已停止")
}

func banner(cfg server.Config, addr string) {
	port := portOf(addr)
	log.Println("QuickShare 已启动")
	log.Printf("  版本       %s", version.String())
	log.Printf("  数据目录   %s", cfg.DataDir)
	if cfg.DataDirLocked {
		log.Printf("             （由启动参数指定，设置界面里只读）")
	} else if p, err := configPath(); err == nil {
		log.Printf("             （可在设置界面里修改，记录在 %s）", p)
	}
	log.Printf("  分片大小   %d MiB", cfg.ChunkSize>>20)
	if cfg.AdminToken == "" {
		log.Printf("  管理口令   未设置（内网免鉴权，公网使用请务必设置 QS_ADMIN_TOKEN）")
	} else {
		log.Printf("  管理口令   已启用")
	}
	log.Printf("  本机访问   %s", localURLOf(addr))
	for _, ip := range lanIPs() {
		log.Printf("  内网访问   http://%s:%s", ip, port)
	}
}

// portOf 从监听地址里取出端口号，取不出来就原样返回。
func portOf(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	return addr
}

// localURLOf 拼出"本机访问"的地址，托盘菜单的「打开页面」用的也是它。
//
// 默认监听地址是 :8080（绑所有接口），这时 localhost 最自然。但如果用户
// 显式绑定了某个地址（-addr 192.168.1.5:8080），那个端口只在该 IP 上监听，
// localhost 是连不上的——托盘里点「打开页面」会打不开，日志只留一行"打开页面失败"。
// 所以只有 host 为空或通配（0.0.0.0 / ::）时才退回 localhost。
func localURLOf(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// 不是 host:port 形式（例如只写了端口），按"本机 + 端口"处理
		return "http://localhost:" + portOf(addr)
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// permissionHint 是"数据目录写不进去"时给用户的提示。
//
// 容器部署最常撞这堵墙，原因值得说清楚：**bind mount 会把镜像里 /data 的属主
// 整个盖掉**。Dockerfile 里虽然 `chown quickshare:users /data` 过，但那是在镜像
// 层里；一旦用 `-v /宿主机/路径:/data` 挂上来，容器看到的就纯粹是宿主机上那个
// 目录的属主。而 OpenWrt / NAS 上新建目录默认是 root:root，容器又以非 root
// 运行（compose 里默认 1000:1000），于是必然 permission denied。
//
// 顺带一提：如果挂的是 **named volume**（`quickshare-data:/data`）而不是 bind
// mount，Docker 首次创建卷时会把镜像里 /data 的内容和属主一起复制过去，就不会
// 有这个问题——代价是文件不在你能直接翻的目录里。
const permissionHint = `  这是权限问题，不是程序问题。
  若在容器里运行：bind mount（-v 宿主机路径:/data）会覆盖镜像内 /data 的属主，
  容器以非 root 运行时，能不能写完全由宿主机上那个目录的属主决定。
  OpenWrt / NAS 上新建目录默认是 root:root，直接挂进来必然写不了。
  两种解法（选一个）：
    1) 宿主机上 chown -R <PUID>:<PGID> <数据目录>，与 compose 里的 user: 保持一致
       （OpenWrt 上 chown 用数字 uid 就行，不必先在 /etc/passwd 里建用户）
    2) 在 docker-compose.yml 里把 user: 改成 "0:0"，让容器以 root 跑
       （内网自用够安全，也是 OpenWrt 上最省事的做法）`

// lanIPs 列出本机的内网 IPv4 地址。
func lanIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || !ip4.IsPrivate() {
				continue
			}
			out = append(out, ip4.String())
		}
	}
	return out
}

// updateTarget 决定要不要做更新检查、问哪个源、查哪个仓库。
//
// 默认问 GitHub，仓库取构建时注入的 ${{ github.repository }}。**本地构建注入的是
// 空串，也就是不检查** —— 自己编的二进制不该去问远端有没有新版本。
//
// 返回的 (source, repo) 里 **repo 为空就是不检查**（此时 source 仍会返回默认源，
// 无意义）。判据是 repo 而不是 source：`newUpdateChecker` 在 repo 为空时会在发请求
// 之前短路，`/api/version` 也如实回 `enabled: false`。
//
//	QS_UPDATE_SOURCE  问哪个源：github（默认）或 dockerhub
//	QS_UPDATE_REPO    那个源里的 "owner/name"；不设则用注入的 GitHub 仓库
//	QS_UPDATE_CHECK=0 整个关掉——内网完全不出网的环境里，开着它只会让每次打开
//	                  设置面板都白等一个必然超时的请求
//
// **为什么需要 dockerhub 这个源**：未认证请求查 GitHub 私有仓库一律 404，
// 而 Docker Hub 的公开仓库不认证也能读 tag 列表。用 Docker 部署、又不想把
// GitHub 仓库公开的话，就靠它。
func updateTarget() (source, repo string) {
	if !envBool("QS_UPDATE_CHECK", true) {
		return "", ""
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv("QS_UPDATE_SOURCE"))) {
	case "", server.SourceGitHub:
		source = server.SourceGitHub
	case server.SourceDockerHub:
		source = server.SourceDockerHub
	default:
		fmt.Fprintf(os.Stderr, "环境变量 QS_UPDATE_SOURCE 只认 %q / %q，无法识别，按 %q 处理\n",
			server.SourceGitHub, server.SourceDockerHub, server.SourceGitHub)
		source = server.SourceGitHub
	}

	repo = strings.TrimSpace(os.Getenv("QS_UPDATE_REPO"))
	if repo != "" {
		return source, repo
	}
	if source == server.SourceDockerHub {
		// **绝不能回落到 version.Repo**：那是 GitHub 的仓库名，放到 Docker Hub 的
		// 命名空间下一定查不到（两边用户名都可能不同），结果就是一个永远查不到、
		// 又看不出原因的检查。宁可关掉并在日志里说清楚。
		fmt.Fprintf(os.Stderr,
			"QS_UPDATE_SOURCE=%s 但没设 QS_UPDATE_REPO（要写成 Docker Hub 的 用户名/镜像名），更新检查已关闭\n",
			server.SourceDockerHub)
		return "", ""
	}
	return source, version.Repo
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "环境变量 %s 不是合法数字，使用默认值 %d\n", key, def)
		return def
	}
	return n
}

// envBool 解析布尔型环境变量，认 1/0、true/false、yes/no、on/off。
func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	fmt.Fprintf(os.Stderr, "环境变量 %s 不是合法布尔值，使用默认值 %v\n", key, def)
	return def
}
