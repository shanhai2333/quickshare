package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"quickshare/internal/store"
)

// maxBackgroundSize 背景图大小上限。背景图只是装饰，10 MiB 足够放一张
// 4K 壁纸，再大只会拖慢首屏。
const maxBackgroundSize = 10 << 20

// 外观数值的取值范围。
//
// 服务端必须自己校验：前端滑块能拖到哪儿不算数，直接 PUT 一个越界值进来，
// 会把 CSS 变量写成垃圾（比如 blur(-5px) 或 alpha(3)），整个页面配色就废了。
const (
	maxBlur        = 40  // 背景图模糊半径上限（px）
	minOpacity     = 20  // 不透明度下限（%）。再低文字就压在背景图上读不清了
	maxOpacity     = 100 // 不透明度上限（%）
	defaultBlur    = 0
	defaultOpacity = 85
)

// allowedBackground 允许作为背景图的类型白名单。
//
// 刻意不包含 image/svg+xml：SVG 可以内嵌 <script>，从同源加载等于自己
// 开了一个 XSS 入口。这里也不信任浏览器声明的 Content-Type，一律用
// http.DetectContentType 嗅探真实字节。
var allowedBackground = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",
	"image/avif": ".avif",
}

// ---------------------------------------------------------------- 首页

// servePage 输出一个 HTML 页面，并把当前主题直接写进 <html> 的 data-theme 属性。
//
// 主题存在服务端，如果只靠前端拉完设置再套上，深色用户每次刷新都会先看到一帧
// 浅色再翻黑。这里在服务端就把属性写进 HTML，首帧即最终配色，不依赖 JS。
//
// 背景图、模糊度、不透明度则留给 JS 设置——它们只在有背景图时才有意义，
// 而首帧本来就要等图片下载，先套上反而是无意义的闪烁。
//
// 两个页面（首页、文本页）都走这里：主题注入和资源版本号必须一致，
// 各写一套的话迟早会出现"某个页面闪屏"或"某个页面吃到旧 JS"。
func (s *Server) servePage(w http.ResponseWriter, r *http.Request, name string) {
	page, err := fs.ReadFile(s.web, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	kv, err := s.be().st.GetSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 只接受这两个字面量，就算数据库被人手改脏了也注不进 HTML
	if theme := kv[store.SettingTheme]; theme == "light" || theme == "dark" {
		page = bytes.Replace(page, []byte(`<html lang="zh-CN">`),
			[]byte(`<html lang="zh-CN" data-theme="`+theme+`">`), 1)
	}

	// 没设访问口令时（内网自用的默认情况），把文本页那两个区块的首帧状态改成"可见"。
	//
	// 它们在 text.html 里默认 hidden，是给"开了口令"的部署用的——不能让未鉴权的人
	// 看到输入框。但没开口令是绝大多数情况，那种情况下让它们等 JS 跑完
	// 「config → settings → 三个并行请求」三轮往返才露面，切页时会先看到一大片
	// 空白的主区。实测（注入 120ms 延迟模拟 NAS + WiFi）：顶栏 157ms 就画出来了，
	// 主区一直空到 525ms，**空白 368ms**；而首页的卡片本来就没带 hidden，
	// 所以"切回文件页"反而不空白——这个不对称就是"切页闪一下"的主因。
	//
	// 按字面量替换：text.html 里那两行有注释盯着，改属性顺序会让这里**静默失效**
	// （页面照样能跑，只是空白又回来了），所以 e2e 里有一条断言直接盯渲染出来的 HTML。
	if s.cfg.AdminToken == "" {
		page = bytes.ReplaceAll(page, []byte(` id="composeCard" hidden`),
			[]byte(` id="composeCard"`))
		page = bytes.ReplaceAll(page, []byte(` id="textsCard" hidden`),
			[]byte(` id="textsCard"`))
	}

	// 前端资源带上内容哈希，升级后浏览器不会拿旧 JS/CSS 去配新服务端
	page = bytes.ReplaceAll(page, []byte("/style.css"), []byte("/style.css?v="+s.assets))
	page = bytes.ReplaceAll(page, []byte("/app.js"), []byte("/app.js?v="+s.assets))
	page = bytes.ReplaceAll(page, []byte("/live.js"), []byte("/live.js?v="+s.assets))
	page = bytes.ReplaceAll(page, []byte("/text.js"), []byte("/text.js?v="+s.assets))
	page = bytes.ReplaceAll(page, []byte("/settings.js"), []byte("/settings.js?v="+s.assets))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache") // 主题随时会变，页面本身不能长缓存
	_, _ = w.Write(page)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.servePage(w, r, "index.html")
}

func (s *Server) handleTextPage(w http.ResponseWriter, r *http.Request) {
	s.servePage(w, r, "text.html")
}

// ---------------------------------------------------------------- 设置

// handleGetSettings 返回界面设置：外观（主题、背景图、透明度）+ 文本保留时长。
//
// 这是个公开接口：主题和背景图必须在鉴权之前就能应用，否则开了口令的
// 部署每次刷新都会先闪一下默认配色，体验很差。设置内容本身不敏感。
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	kv, err := s.be().st.GetSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.settingsDTO(kv))
}

// handlePutSettings 写入设置（外观 + 文本保留时间）。字段都是可选的，只更新传进来的那些。
//
// 用指针接收是为了区分"没传这个字段"和"传了空值"：theme 的空字符串
// 表示"跟随系统"，是个合法值，不能和"没传"混为一谈。textTTL 也一样——
// value 的 0 表示"不自动清理"，是合法值。
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	b := s.be()
	var in struct {
		Theme   *string `json:"theme"`
		BgBlur  *int    `json:"bgBlur"`
		Opacity *struct {
			Topbar *int `json:"topbar"`
			Upload *int `json:"upload"`
			Files  *int `json:"files"`
		} `json:"opacity"`
		// 文本保留时长。value 为 0 表示永不自动删除（默认）。
		TextTTL *ttlInput `json:"textTTL"`
		// 文件保留时长。value 为 0 表示永不自动删除（默认）。
		FileTTL *ttlInput `json:"fileTTL"`
		// 验证码文本的保留时长。**这里的 0 是"关掉这条规则"**（验证码按普通
		// 文本处理），不是"永不删除"——所以它跟上面两档的 0 含义不同。
		CodeTTL *ttlInput `json:"codeTTL"`
		// 设备随消息删除：删文本时顺手把已经没有任何文本的设备记录也删掉。
		PruneDevices *bool `json:"pruneDevices"`
		// 分片大小只影响新建上传任务；进行中的任务继续使用它自己记录的大小。
		ChunkSize *int `json:"chunkSize"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}

	updates := map[string]string{}

	if in.Theme != nil {
		// 空字符串表示"跟随系统"，是合法值
		if v := *in.Theme; v != "" && v != "light" && v != "dark" {
			writeErr(w, http.StatusBadRequest, "theme 只能是 light、dark 或空字符串")
			return
		}
		updates[store.SettingTheme] = *in.Theme
	}

	if in.BgBlur != nil {
		if v := *in.BgBlur; v < 0 || v > maxBlur {
			writeErr(w, http.StatusBadRequest,
				"bgBlur 取值范围是 0 到 "+strconv.Itoa(maxBlur))
			return
		}
		updates[store.SettingBgBlur] = strconv.Itoa(*in.BgBlur)
	}

	if o := in.Opacity; o != nil {
		for _, f := range []struct {
			key string
			val *int
		}{
			{store.SettingOpacityTopbar, o.Topbar},
			{store.SettingOpacityUpload, o.Upload},
			{store.SettingOpacityFiles, o.Files},
		} {
			if f.val == nil {
				continue
			}
			if v := *f.val; v < minOpacity || v > maxOpacity {
				writeErr(w, http.StatusBadRequest,
					"不透明度取值范围是 "+strconv.Itoa(minOpacity)+" 到 "+strconv.Itoa(maxOpacity))
				return
			}
			updates[f.key] = strconv.Itoa(*f.val)
		}
	}

	for _, f := range []struct {
		in       *ttlInput
		valueKey string
		unitKey  string
		zeroNote string
	}{
		{in.TextTTL, store.SettingTextTTLValue, store.SettingTextTTLUnit, "0 表示永不删除"},
		{in.FileTTL, store.SettingFileTTLValue, store.SettingFileTTLUnit, "0 表示永不删除"},
		{in.CodeTTL, store.SettingCodeTTLValue, store.SettingCodeTTLUnit, "0 表示关掉这条规则"},
	} {
		if msg := applyTTL(updates, f.in, f.valueKey, f.unitKey, f.zeroNote); msg != "" {
			writeErr(w, http.StatusBadRequest, msg)
			return
		}
	}

	if in.PruneDevices != nil {
		updates[store.SettingPruneDevices] = boolSetting(*in.PruneDevices)
	}

	if in.ChunkSize != nil {
		if *in.ChunkSize < int(minChunkSize) || *in.ChunkSize > int(maxChunkSize) {
			writeErr(w, http.StatusBadRequest, "分片大小取值范围是 1 到 64 MiB")
			return
		}
		updates[store.SettingChunkSize] = strconv.Itoa(*in.ChunkSize)
	}

	// 先全部校验完再落库，避免"一半写进去了、一半被拒"这种半截状态
	for k, v := range updates {
		if err := b.st.SetSetting(k, v); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// 刚打开「设备随消息删除」时，把**已经**空掉的设备记录立刻清一遍。
	//
	// 不这么做的话，用户打开开关、回到列表一看还是那一堆空设备，会以为开关
	// 没生效——而它其实只对"以后"的删除生效。用户要的恰恰是"删过的消息别再
	// 留着设备"。放在落库之后调，这样 pruneDevicesIfEnabled 读到的就是新值。
	if in.PruneDevices != nil && *in.PruneDevices {
		s.pruneDevicesIfEnabled()
	}

	kv, err := b.st.GetSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.settingsDTO(kv))
}

func (s *Server) settingsDTO(kv map[string]string) map[string]any {
	out := map[string]any{
		"theme":      kv[store.SettingTheme],
		"background": nil,
		"bgBlur":     intOr(kv[store.SettingBgBlur], defaultBlur),
		"opacity": map[string]int{
			"topbar": intOr(kv[store.SettingOpacityTopbar], defaultOpacity),
			"upload": intOr(kv[store.SettingOpacityUpload], defaultOpacity),
			"files":  intOr(kv[store.SettingOpacityFiles], defaultOpacity),
		},
		// 单位认不出来（含从没设过）时回落到该档的默认单位：值为 0 才是"不清理"，
		// 单位本身不该成为"设了却不生效"的原因。
		"textTTL": map[string]any{
			"value": intOr(kv[store.SettingTextTTLValue], 0),
			"unit":  ttlUnitOr(kv[store.SettingTextTTLUnit], "day"),
		},
		"fileTTL": map[string]any{
			"value": intOr(kv[store.SettingFileTTLValue], 0),
			"unit":  ttlUnitOr(kv[store.SettingFileTTLUnit], "day"),
		},
		"codeTTL": map[string]any{
			"value": codeTTLValueOr(kv),
			"unit":  ttlUnitOr(kv[store.SettingCodeTTLUnit], "minute"),
		},
		"pruneDevices": kv[store.SettingPruneDevices] == "1",
		"chunkSize":    s.chunkSize(),
	}
	if _, err := os.Stat(s.be().bgPath()); err == nil {
		out["background"] = map[string]any{
			// 带版本号查询参数，内容一变 URL 就变，可以放心长缓存
			"url":  "/api/background?v=" + kv[store.SettingBgVersion],
			"mime": kv[store.SettingBgMime],
		}
	}
	return out
}

// codeTTLValueOr 取验证码保留时长的数值。
//
// 跟另外两档不同：键**不存在**时给默认值 10（分钟），而键存在且为 0 表示
// 关掉这条规则。所以不能直接用 intOr —— 它分不清"没设过"和"设成 0"。
func codeTTLValueOr(kv map[string]string) int {
	if strings.TrimSpace(kv[store.SettingCodeTTLValue]) == "" {
		return int(defaultCodeTTL / time.Minute)
	}
	return intOr(kv[store.SettingCodeTTLValue], 0)
}

// ttlInput 是请求体里一档保留时长的形状。两个字段都可选（指针），
// 只传一个时另一个保持原样——用户往往只想改数值、不想动单位。
type ttlInput struct {
	Value *int    `json:"value"`
	Unit  *string `json:"unit"`
}

// applyTTL 校验一档保留时长并写进 updates，返回错误信息（空串表示通过）。
//
// zeroNote 是"0 代表什么"的说明，只用来拼报错文案：文本和文件那两档的 0 是
// "永不删除"，验证码那档的 0 是"关掉规则"，文案说错会让人按错的方向理解。
func applyTTL(updates map[string]string, in *ttlInput, valueKey, unitKey, zeroNote string) string {
	if in == nil {
		return ""
	}
	if in.Value != nil {
		// 0 是合法值，含义见 zeroNote
		if v := *in.Value; v < 0 || v > maxTTLValue {
			return "保留时长取值范围是 0 到 " + strconv.Itoa(maxTTLValue) +
				"（" + zeroNote + "）"
		}
		updates[valueKey] = strconv.Itoa(*in.Value)
	}
	if in.Unit != nil {
		if !validTTLUnit(*in.Unit) {
			return "单位只能是 minute、hour 或 day"
		}
		updates[unitKey] = *in.Unit
	}
	return ""
}

// validTTLUnit 判断单位认不认识。
//
// 加 "minute" 是为了验证码那档：默认 10 分钟用"天"根本表达不出来。
// 顺带让文本和文件那两档也能填分钟——它们本来就有短保留的需求。
func validTTLUnit(u string) bool {
	return u == "minute" || u == "hour" || u == "day"
}

// ttlUnitOr 取单位，认不出来就用该档的默认值。
func ttlUnitOr(v, def string) string {
	if validTTLUnit(v) {
		return v
	}
	return def
}

// intOr 把设置里存的字符串转成整数，解析不了就退回默认值。
func intOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// boolSetting 把布尔开关存成 "1" / "0"。
//
// 存成 "1" 而不是 Go 的 "true"：这个键只有两个值，短一点，
// 也和别处"值就是给 strconv 用的字符串"保持一致。读的时候只认 "1"，
// 其余一律当关闭——**认不出来就当关闭**是这里的正确默认（多删一次设备记录
// 比少删一次难解释）。
func boolSetting(on bool) string {
	if on {
		return "1"
	}
	return "0"
}

// pruneDevicesOn 读「设备随消息删除」这个开关。
//
// 读失败当关闭：这个开关只影响"要不要顺手清设备记录"，读不出来就什么都不做，
// 绝不能因为读设置出错反而把设备删了。
func (s *Server) pruneDevicesOn() bool {
	kv, err := s.be().st.GetSettings()
	if err != nil {
		log.Printf("读取设备清理设置失败: %v", err)
		return false
	}
	return kv[store.SettingPruneDevices] == "1"
}

// pruneDevicesIfEnabled 按开关决定要不要清掉已经没有文本的设备记录。
//
// 删文本之后调用。清掉之后要推一次 texts 主题：设备列表和文本行里的显示名
// 都变了，打开的页面得跟着重拉。
func (s *Server) pruneDevicesIfEnabled() {
	if !s.pruneDevicesOn() {
		return
	}
	n, err := s.be().st.PruneOrphanDevices()
	if err != nil {
		log.Printf("清理空设备记录失败: %v", err)
		return
	}
	if n > 0 {
		log.Printf("已清理 %d 台没有文本的设备", n)
		s.events.notify(topicTexts)
	}
}

// ---------------------------------------------------------------- 背景图

// handleGetBackground 输出当前背景图。
func (s *Server) handleGetBackground(w http.ResponseWriter, r *http.Request) {
	b := s.be()
	kv, err := b.st.GetSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	mime := kv[store.SettingBgMime]
	if mime == "" {
		writeErr(w, http.StatusNotFound, "未设置背景图")
		return
	}

	f, err := os.Open(b.bgPath())
	if err != nil {
		writeErr(w, http.StatusNotFound, "未设置背景图")
		return
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	ver := kv[store.SettingBgVersion]
	w.Header().Set("Content-Type", mime)
	// URL 里带了内容哈希，同一版本的内容永不变，可以标 immutable
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+ver+`"`)
	http.ServeContent(w, r, "background", fi.ModTime(), f)
}

// handleUploadBackground 接收背景图。请求体就是图片原始字节，
// Content-Type 仅作参考，实际类型以字节嗅探为准。
func (s *Server) handleUploadBackground(w http.ResponseWriter, r *http.Request) {
	b := s.be()
	// 用 LimitReader 而不是 MaxBytesReader：后者会标记连接不可复用，
	// 在 HTTP/2 下有时会连响应一起丢掉，这里只需要自己判断长度。
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBackgroundSize+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取上传内容失败")
		return
	}
	if len(data) == 0 {
		writeErr(w, http.StatusBadRequest, "内容为空")
		return
	}
	if len(data) > maxBackgroundSize {
		writeErr(w, http.StatusRequestEntityTooLarge,
			"背景图不能超过 "+humanSize(maxBackgroundSize))
		return
	}

	mime := http.DetectContentType(data)
	if _, ok := allowedBackground[mime]; !ok {
		writeErr(w, http.StatusUnsupportedMediaType,
			"只支持 JPG / PNG / WebP / GIF / AVIF 格式的图片")
		return
	}

	// 先写临时文件再原子改名，避免上传中断时留下半张图
	tmp := b.bgPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, "写入背景图失败: "+err.Error())
		return
	}
	if err := os.Rename(tmp, b.bgPath()); err != nil {
		_ = os.Remove(tmp)
		writeErr(w, http.StatusInternalServerError, "保存背景图失败: "+err.Error())
		return
	}

	sum := sha256.Sum256(data)
	ver := hex.EncodeToString(sum[:8])
	if err := b.st.SetSetting(store.SettingBgMime, mime); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := b.st.SetSetting(store.SettingBgVersion, ver); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	kv, err := b.st.GetSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.settingsDTO(kv))
}

// handleDeleteBackground 移除背景图。
func (s *Server) handleDeleteBackground(w http.ResponseWriter, r *http.Request) {
	b := s.be()
	if err := os.Remove(b.bgPath()); err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, "删除背景图失败: "+err.Error())
		return
	}
	_ = b.st.DeleteSetting(store.SettingBgMime)
	_ = b.st.DeleteSetting(store.SettingBgVersion)

	kv, err := b.st.GetSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.settingsDTO(kv))
}
