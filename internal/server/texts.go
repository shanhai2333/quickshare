package server

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"quickshare/internal/store"
)

// 文本与设备的各项上限。
//
// 这些必须在服务端生效：前端的 maxlength / disabled 只是提示，
// 直接 curl 一个 POST 进来完全不受它们约束。
const (
	// maxTextLen 单条文本长度上限。这是"发一段文本过去"用的便签，不是网盘：
	// 32 KiB 够贴一屏长文、一段配置、一个长链接；再大就该走文件上传那条路
	// （那边有分片、断点续传和磁盘配额）。
	maxTextLen = 32 << 10
	// maxRemarkLen 设备备注长度上限。它是列表里的标签，不是正文。
	maxRemarkLen = 40
	// maxUALen 入库 UA 的截断长度。UA 可以很长（某些 App 内嵌浏览器能到几百字节），
	// 我们只需要从中解析出设备名，存太多没意义。
	maxUALen = 300
)

// unknownDevice 是解析不出设备时的显示名。
const unknownDevice = "未知设备"

// ---------------------------------------------------------------- 文本

// textDTO 是返回给前端的文本视图。
//
// DeviceName 由服务端算好：UA 解析的逻辑只该有一份，放前端的话
// 首页和文本页迟早各写一套、解析结果还不一样。
type textDTO struct {
	ID         string `json:"id"`
	Content    string `json:"content"`
	DeviceID   string `json:"deviceId"`
	DeviceName string `json:"deviceName"`
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  int64  `json:"updatedAt"`
}

func (s *Server) handleListTexts(w http.ResponseWriter, r *http.Request) {
	b := s.be()
	texts, err := b.st.ListTexts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 设备表整张读出来建索引，而不是逐条文本去查它的设备：
	// 后者是标准的 N+1，文本一多就会变成几百次查询。
	devices, err := b.st.ListDevices()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	byID := make(map[string]*store.Device, len(devices))
	for _, d := range devices {
		byID[d.ID] = d
	}

	out := make([]textDTO, 0, len(texts))
	for _, t := range texts {
		out = append(out, textDTO{
			ID:         t.ID,
			Content:    t.Content,
			DeviceID:   t.DeviceID,
			DeviceName: deviceLabel(byID[t.DeviceID]),
			CreatedAt:  t.CreatedAt.Unix(),
			UpdatedAt:  t.UpdatedAt.Unix(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateText 新建一条文本。
//
// 顺带把发送方记进设备表：设备信息只在"它真的发了点什么"的时候才有意义，
// 没必要为每个打开页面的人建一条记录（那会让列表被一堆没发过东西的设备塞满）。
//
// 设备身份取自连接来源 IP，**不接受请求体里的 deviceId**（见 clientIP 的说明）。
func (s *Server) handleCreateText(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content string `json:"content"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}

	// 只判"是不是全是空白"，但存原样：用户贴的代码块前后缩进是有意义的，
	// 替他把首尾空白抹掉等于改了他的内容。
	if strings.TrimSpace(in.Content) == "" {
		writeErr(w, http.StatusBadRequest, "内容不能为空")
		return
	}
	if len(in.Content) > maxTextLen {
		writeErr(w, http.StatusRequestEntityTooLarge,
			"文本不能超过 "+humanSize(maxTextLen))
		return
	}

	// 让前端传 deviceId 的话，任何人 curl 一下就能报上别人的 ID，
	// 冒充别的设备发文本、甚至借"改备注"把别人的设备名改掉。
	ip := clientIP(r)

	b := s.be()
	if ip != "" {
		// UA 同样从请求头读，不信前端传的。
		if err := b.st.TouchDevice(ip, cutUTF8(r.UserAgent(), maxUALen)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	id, err := randomToken(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成 ID 失败")
		return
	}

	now := time.Now()
	t := &store.Text{
		ID:        id,
		Content:   in.Content,
		DeviceID:  ip,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := b.st.CreateText(t); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.events.notify(topicTexts)
	writeJSON(w, http.StatusOK, textDTO{
		ID: t.ID, Content: t.Content, DeviceID: t.DeviceID,
		DeviceName: s.deviceLabelOf(t.DeviceID),
		CreatedAt:  t.CreatedAt.Unix(), UpdatedAt: t.UpdatedAt.Unix(),
	})
}

// handleUpdateText 改一条文本的内容。
func (s *Server) handleUpdateText(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content string `json:"content"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	if strings.TrimSpace(in.Content) == "" {
		writeErr(w, http.StatusBadRequest, "内容不能为空")
		return
	}
	if len(in.Content) > maxTextLen {
		writeErr(w, http.StatusRequestEntityTooLarge,
			"文本不能超过 "+humanSize(maxTextLen))
		return
	}

	b := s.be()
	id := r.PathValue("id")
	if err := b.st.UpdateText(id, in.Content); err != nil {
		writeStoreErr(w, err, "文本不存在")
		return
	}

	t, err := b.st.GetText(id)
	if err != nil {
		writeStoreErr(w, err, "文本不存在")
		return
	}

	s.events.notify(topicTexts)
	writeJSON(w, http.StatusOK, textDTO{
		ID: t.ID, Content: t.Content, DeviceID: t.DeviceID,
		DeviceName: s.deviceLabelOf(t.DeviceID),
		CreatedAt:  t.CreatedAt.Unix(), UpdatedAt: t.UpdatedAt.Unix(),
	})
}

func (s *Server) handleDeleteText(w http.ResponseWriter, r *http.Request) {
	if err := s.be().st.DeleteText(r.PathValue("id")); err != nil {
		writeStoreErr(w, err, "文本不存在")
		return
	}
	s.events.notify(topicTexts)
	// 开着「设备随消息删除」时，顺手清掉已经没有文本的设备记录
	s.pruneDevicesIfEnabled()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// maxBatchDelete 一次批量删除的 ID 数上限。
//
// 前端"全选"传过来的是当前列表里的全部 ID，正常也就几十上百条。设上限是为了
// 别让一个手写的请求塞进十万个 ID，把 SQL 拼成几百 KB 的语句。
const maxBatchDelete = 1000

// handleDeleteTexts 批量删除。
//
// 单独一个 POST 而不是 `DELETE /api/texts` 带 body：后者虽然不少客户端支持，
// 但在 HTTP 语义里是灰色地带，中间设备（反代、缓存）对带 body 的 DELETE 处理
// 不一致，容易被吃掉 body。
func (s *Server) handleDeleteTexts(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	if len(in.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "没有指定要删除的文本")
		return
	}
	if len(in.IDs) > maxBatchDelete {
		writeErr(w, http.StatusBadRequest,
			"一次最多删除 "+strconv.Itoa(maxBatchDelete)+" 条")
		return
	}

	n, err := s.be().st.DeleteTexts(in.IDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.events.notify(topicTexts)
	// 开着「设备随消息删除」时，顺手清掉已经没有文本的设备记录。
	// 放在 notify 之后：两次 notify 都是同一个主题，前端的防抖会合并成一次拉取。
	s.pruneDevicesIfEnabled()
	// 回实际删掉的条数：某几条可能已被别的设备删掉了，前端据此提示"删了 N 条"
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": n})
}

// ---------------------------------------------------------------- 设备

type deviceDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`   // 备注优先，没有就用 UA 解析出来的
	Remark   string `json:"remark"` // 原始备注，界面里回填输入框用
	UA       string `json:"ua"`
	LastSeen int64  `json:"lastSeen"`
	// TextCount 是这台设备还剩几条文本。
	//
	// 设备记录不随文本删除而消失，所以界面上得能看出"这台其实已经空了"——
	// 否则用户只能靠"记得自己删过"来判断该不该删设备。删之前那句确认也要用它。
	TextCount int `json:"textCount"`
	// IsMe 表示这台设备就是发起本次请求的那台。
	//
	// 由服务端算而不是让前端拿自己的 ID 去比：前端根本不需要知道自己的
	// 设备 ID 是什么，也就不必再维护一份"我是谁"的状态——那份状态正是
	// 之前用 localStorage 时会在不同地址之间对不上的东西。
	IsMe bool `json:"isMe"`
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.be().st.ListDevices()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	me := clientIP(r)
	out := make([]deviceDTO, 0, len(devices))
	for _, d := range devices {
		out = append(out, deviceDTO{
			ID: d.ID, Name: deviceLabel(d), Remark: d.Remark, UA: d.UA,
			LastSeen: d.LastSeen.Unix(), TextCount: d.TextCount,
			IsMe: d.ID == me,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteDevice 删掉一台设备的记录（含备注），**不碰它的文本**。
//
// 删掉之后那些文本还在，只是显示名回落成"未知设备"——前端确认框里会说清。
// 之所以不连文本一起删：用户点的是"把这个设备条目去掉"，不是"清空它的内容"，
// 顺手删内容属于替他做了没要求的事，而且删了就找不回来。
func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.be().st.DeleteDevice(id); err != nil {
		writeStoreErr(w, err, "设备不存在")
		return
	}

	// 文本行里显示的就是这台设备的名字，删完要让打开的页面跟着重拉
	s.events.notify(topicTexts)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSetDeviceRemark 改设备备注。传空串表示恢复成 UA 解析出来的名字。
func (s *Server) handleSetDeviceRemark(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Remark string `json:"remark"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}

	remark := strings.TrimSpace(in.Remark)
	if len(remark) > maxRemarkLen {
		writeErr(w, http.StatusBadRequest, "备注不能超过 "+humanSize(maxRemarkLen)+" 字节")
		return
	}

	if err := s.be().st.SetDeviceRemark(r.PathValue("id"), remark); err != nil {
		writeStoreErr(w, err, "设备不存在")
		return
	}

	// 文本列表里显示的就是这个名字，所以改完备注要让它一起刷新
	s.events.notify(topicTexts)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "remark": remark})
}

// deviceLabelOf 按设备 ID 取显示名，查不到就给"未知设备"。
func (s *Server) deviceLabelOf(id string) string {
	if id == "" {
		return unknownDevice
	}
	devices, err := s.be().st.ListDevices()
	if err != nil {
		return unknownDevice
	}
	for _, d := range devices {
		if d.ID == id {
			return deviceLabel(d)
		}
	}
	return unknownDevice
}

// deviceLabel 算出一台设备的显示名：备注优先，否则拿 UA 解析。
func deviceLabel(d *store.Device) string {
	if d == nil {
		return unknownDevice
	}
	if r := strings.TrimSpace(d.Remark); r != "" {
		return r
	}
	return deviceName(d.UA)
}

// ---------------------------------------------------------------- UA 解析

// deviceName 从 UA 里解析出一个可读的设备名，作为没设备注时的默认显示。
//
// 只求"尽量好看"，不求准确——这是个兜底标签，不是身份。真正的身份是设备 ID：
// 两台同型号手机解析出来都是 "Android 手机 · Chrome"，但它们的 ID 不同，
// 改备注不会串到对方身上（这正是拿 UA 当身份会踩的坑）。
//
// 认不出来就回"未知设备"，绝不把整条 UA 甩到界面上——那玩意儿又长又没法看。
func deviceName(ua string) string {
	if strings.TrimSpace(ua) == "" {
		return unknownDevice
	}

	var os string
	switch {
	case strings.Contains(ua, "iPhone"):
		os = "iPhone"
	case strings.Contains(ua, "iPad"):
		os = "iPad"
	case strings.Contains(ua, "Android"):
		// UA 里的 "Mobile" 是区分手机与平板的老约定，Android 平板普遍不带
		if strings.Contains(ua, "Mobile") {
			os = "Android 手机"
		} else {
			os = "Android 平板"
		}
	case strings.Contains(ua, "Windows"):
		os = "Windows"
	case strings.Contains(ua, "Macintosh"), strings.Contains(ua, "Mac OS X"):
		os = "Mac"
	case strings.Contains(ua, "Linux"):
		os = "Linux"
	}

	// 浏览器要按这个顺序判：Edge 的 UA 里同时含 "Chrome" 和 "Safari"，
	// Opera 含 "Chrome"，Chrome 含 "Safari"。先判宽的那个会把它们全认成 Chrome。
	var browser string
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "OPR/"), strings.Contains(ua, "Opera"):
		browser = "Opera"
	case strings.Contains(ua, "Chrome/"), strings.Contains(ua, "CriOS"):
		browser = "Chrome"
	case strings.Contains(ua, "Firefox/"), strings.Contains(ua, "FxiOS"):
		browser = "Firefox"
	case strings.Contains(ua, "Version/"), strings.Contains(ua, "Safari/"):
		browser = "Safari"
	case strings.Contains(ua, "curl/"):
		browser = "curl"
	}

	switch {
	case os != "" && browser != "":
		return os + " · " + browser
	case os != "":
		return os
	case browser != "":
		return browser
	default:
		return unknownDevice
	}
}

// ---------------------------------------------------------------- 设备身份

// localDeviceID 是"请求来自本机"时统一使用的设备 ID。
//
// 为什么需要它：客户端从**本机的内网 IP** 访问服务时，连接的源地址就是那个内网 IP，
// 而不是 127.0.0.1。同一台机器于是会分裂成两台设备——用户在浏览器里用 127.0.0.1
// 打开，又从启动日志（或二维码）点了一次内网 IP 的地址，设备列表里就多出一条。
// 这两个地址指向同一个服务、同一台机器，必须归一。
//
// 用 "localhost" 而不是某个具体 IP 当 ID：从内网 IP 进来时显示 127.0.0.1 会让人困惑，
// 而 "localhost" 谁看都知道是本机；它同时也是 URL 安全的 ASCII。
const localDeviceID = "localhost"

var (
	localAddrsMu  sync.Mutex
	localAddrsSet map[string]bool
	localAddrsAt  time.Time
)

// 网卡地址表的保鲜期。超过它再查就重扫一次。
const localAddrsTTL = 2 * time.Second

// isLocalAddr 判断一个地址是不是本机自己的接口地址（含回环）。
//
// **刻意不用 sync.Once**。原来是用 Once 把网卡列表只算一次的，理由是"服务端自己的
// 地址极少变化"。但启动那一刻网卡可能还没拿到地址——NAS / 路由器上开机自启就是这个
// 时序，而 Once 会把那份**不完整的**列表永久钉住：之后用内网 IP 访问永远认不出是本机，
// 同一台机器又分裂成两台设备。那正是 clientIP 那段注释里要修的 bug，换了个入口回来，
// 而且不会自愈。
//
// 现在带保鲜期，并且**没命中时无条件重扫一次**：开机时序错了也能在第一个请求上自愈。
// 重扫就是一次 getifaddrs，这点代价可以忽略。
func isLocalAddr(host string) bool {
	ip := net.ParseIP(normalizeHost(host))
	if ip == nil {
		return false
	}
	// 回环要按**网段**判，不能只认 127.0.0.1：整个 127.0.0.0/8 都是回环，
	// 而 127.0.0.2 这类地址在测试里很常用（见 verify-device-identity.mjs）。
	if ip.IsLoopback() {
		return true
	}
	key := ip.String()
	if localAddrCached(key, false) {
		return true
	}
	// 没命中。可能是表过期了（网卡刚拿到地址），也可能确实是外部地址——分不出来，
	// 就重扫一次再判。外部地址的请求本来就少，这点代价换的是"不用重启就能自愈"。
	return localAddrCached(key, true)
}

// localAddrCached 查网卡地址表，force 为真时无条件重扫。
func localAddrCached(key string, force bool) bool {
	localAddrsMu.Lock()
	defer localAddrsMu.Unlock()

	if force || localAddrsSet == nil || time.Since(localAddrsAt) >= localAddrsTTL {
		set := map[string]bool{}
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if ipnet, ok := a.(*net.IPNet); ok {
					set[ipnet.IP.String()] = true
				}
			}
		}
		localAddrsSet = set
		localAddrsAt = time.Now()
	}
	return localAddrsSet[key]
}

// normalizeHost 把地址规范成可比较的形式：去掉 IPv6 的 zone 标记
// （fe80::1%eth0 → fe80::1），并把 IP 转成规范写法——`::1` 有好几种合法写法，
// 不统一就比对不上。
func normalizeHost(host string) string {
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

// clientIP 取发起请求的客户端 IP，它就是设备身份。
//
// **为什么不用浏览器生成的随机串**：那种串只能存在 localStorage 里，而
// localStorage 严格按 origin 隔离。同一个服务，用 127.0.0.1、localhost、
// 内网 IP 打开就是三个不同的 origin，各存各的随机串——同一台机器会被记成
// 三台设备。用户"新开一个窗口"（顺手换了个地址）就会看到设备列表里多一条。
//
// **代价**：同一个 NAT / 手机热点后面共享出口 IP 的多台设备会被合并成一台。
// 内网直连场景下每台设备有自己的 IP，这个取舍是划算的；真要再细分，
// 用户还能自己改备注。
//
// 刻意**不看 X-Forwarded-For**：内网直连时它本来就是空的，而一旦信任它，
// 任何客户端都能随手编一个来冒充别的设备——等于把身份又交回给请求方。
// 将来真要放到反向代理后面，必须改成"只信明确配置过的代理地址"。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// 正常总是 "IP:端口"。真解析不出来就原样用；空串会让调用方跳过登记。
		host = r.RemoteAddr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	// 本机自己的地址（回环、以及本机所有网卡地址）统一归到一个 ID。
	// 少了这一步，"用 127.0.0.1 打开"和"用内网 IP 打开"仍是两台设备。
	if isLocalAddr(host) {
		return localDeviceID
	}
	return normalizeHost(host)
}

// ---------------------------------------------------------------- 工具

// cutUTF8 按字符边界截断字符串。
//
// 直接写 s[:n] 是按字节切，一个汉字占 3 字节，切在第 n 个字节就把它劈成半个，
// 落库就是非法 UTF-8，界面上显示成一个方块。（SanitizeName 踩过同一个坑。）
func cutUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := 0
	for i := range s { // range 给出的 i 总是字符的起始字节
		if i > maxBytes {
			break
		}
		cut = i
	}
	return s[:cut]
}

// writeStoreErr 把 store 层的错误映射成 HTTP 响应。
func writeStoreErr(w http.ResponseWriter, err error, notFoundMsg string) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, notFoundMsg)
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

// ---------------------------------------------------------------- 文本保留时长

// TTL 的取值上限。数值本身不大（最多 10000），真正的约束是别让人填一个
// 天文数字把 time.Duration 乘溢出。
const maxTTLValue = 10000

// textTTLFrom 从设置里解析出文本保留时长。
//
// bool 为 false 表示**没有启用自动清理**——值为空、解析不了、或者 <= 0 都算没启用。
// 这个方向是刻意保守的：宁可留着让用户手动删，也不要因为一个解析不了的值
// 就把人家攒的文本清空。
func textTTLFrom(kv map[string]string) (time.Duration, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(kv[store.SettingTextTTLValue]))
	if err != nil || n <= 0 {
		return 0, false
	}
	unit := kv[store.SettingTextTTLUnit]
	if unit == "" {
		// 只填了数值、没带单位（手写的请求）时按天算，别让它静默地不生效
		unit = "day"
	}
	switch unit {
	case "hour":
		return time.Duration(n) * time.Hour, true
	case "day":
		return time.Duration(n) * 24 * time.Hour, true
	default:
		// 单位认不出来就当没设置。前端只会写这两个值，
		// 走到这儿说明库里的值被人手改脏了，那就不动数据。
		return 0, false
	}
}
