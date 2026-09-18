package server

import (
	"log"
	"net"
	"net/http"
	"strings"
)

// 反向代理下的设备身份。
//
// 背景：设备身份取自连接的源地址（见 clientIP）。直连时这是对的，但放到 Nginx /
// Caddy / Docker Desktop 的 userland-proxy 后面，源地址就变成代理自己的了——所有设备
// 会被合并成一台，"设备备注"那一栏直接失去意义。
//
// 解法不是"无条件读 X-Forwarded-For"：那个头是请求方随便写的，一旦采信，任何人都能
// 报上别人的地址去冒充别的设备，等于把身份又交回给请求方。**只有直连的对端本身是
// 受信代理时**才看它，而受信代理由部署方显式配置（`QS_TRUSTED_PROXIES`）。
//
// 默认不配 = 行为跟以前完全一样，一个字都不读。

// trustedProxies 是解析好的受信代理名单。
type trustedProxies []*net.IPNet

// parseTrustedProxies 解析 `QS_TRUSTED_PROXIES`：逗号分隔，每一项是单个 IP
// （`10.0.0.5`、`::1`）或一个网段（`10.0.0.0/8`、`172.16.0.0/12`）。
//
// 认不出来的项**跳过并打日志**，不让服务起不来：这是个附加能力，配置写错时退化成
// "不信任任何代理"（也就是老行为）比整个服务拒绝启动好得多。
func parseTrustedProxies(spec string) trustedProxies {
	var out trustedProxies
	for _, raw := range strings.Split(spec, ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		// 网段
		if strings.Contains(item, "/") {
			_, ipnet, err := net.ParseCIDR(item)
			if err != nil {
				log.Printf("QS_TRUSTED_PROXIES 里的 %q 不是合法网段，已忽略", item)
				continue
			}
			out = append(out, ipnet)
			continue
		}
		// 单个 IP：按 /32 或 /128 处理。用 ParseCIDR 而不是手搓掩码，
		// 这样 IPv4 / IPv6 走同一条路。
		ip := net.ParseIP(item)
		if ip == nil {
			log.Printf("QS_TRUSTED_PROXIES 里的 %q 不是合法 IP，已忽略", item)
			continue
		}
		bits := 128
		if ip.To4() != nil {
			bits = 32
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out
}

// contains 判断一个地址是否落在名单里。
func (p trustedProxies) contains(host string) bool {
	ip := net.ParseIP(normalizeHost(host))
	if ip == nil {
		return false
	}
	for _, n := range p {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// realClient 从代理头里取出真实客户端地址。取不到就返回空串，调用方退回源地址。
//
// **必须从右往左找，这一条是安全的关键。** 代理是把"它看到的对端地址"**追加**在
// `X-Forwarded-For` 末尾的，所以越靠右越接近我们。客户端自己塞进去的东西只会留在
// **左边**——从左边取等于让请求方自己填身份。
//
//	客户端伪造：X-Forwarded-For: 127.0.0.1        ← 想冒充"本机"
//	代理追加后：X-Forwarded-For: 127.0.0.1, 192.168.1.30
//	                                     ↑ 忽略     ↑ 这才是真的
//
// 从右往左跳过受信代理，第一个不受信的地址就是真实客户端（链上每一跳都是受信代理时
// 才成立，这也正是名单的意义）。认不出是 IP 的条目直接跳过——不能拿一个任意字符串
// 去当设备 ID。
func (p trustedProxies) realClient(remoteAddr string, h http.Header) string {
	peer, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		peer = remoteAddr
	}
	// 直连的对端不是受信代理 → 它写的东西一概不采信。
	if !p.contains(peer) {
		return ""
	}

	if xff := h.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			item := strings.TrimSpace(parts[i])
			if item == "" {
				continue
			}
			ip := net.ParseIP(normalizeHost(item))
			if ip == nil {
				continue // 不是 IP，可能是客户端伪造的字符串（也可能是代理写的 "unknown"）
			}
			if p.contains(ip.String()) {
				continue // 这一跳也是受信代理，继续往左
			}
			return ip.String()
		}
		// 整条链都是受信代理（或者全是垃圾）。**不硬猜**，接着看 X-Real-IP。
	}

	// 有些代理只设 X-Real-IP 不设 X-Forwarded-For（Nginx 的常见写法是两个都设）。
	// 它没有"链"的概念，就是代理看到的对端地址，同样只在受信时才读。
	if real := strings.TrimSpace(h.Get("X-Real-IP")); real != "" {
		if ip := net.ParseIP(normalizeHost(real)); ip != nil {
			return ip.String()
		}
	}
	return ""
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
// 配了 QS_TRUSTED_PROXIES 才会去看 X-Forwarded-For / X-Real-IP，规则见
// trustedProxies.realClient。**默认一个头都不读**——那等于把身份交回给请求方。
func (s *Server) clientIP(r *http.Request) string {
	if len(s.proxies) > 0 {
		if real := s.proxies.realClient(r.RemoteAddr, r.Header); real != "" {
			return deviceIDFor(real)
		}
	}
	return deviceIDFromAddr(r.RemoteAddr)
}

// deviceIDFromAddr 把连接的源地址换算成设备 ID。这是**不经过任何代理头**的那条路。
func deviceIDFromAddr(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// 正常总是 "IP:端口"。真解析不出来就原样用；空串会让调用方跳过登记。
		host = remoteAddr
	}
	return deviceIDFor(strings.TrimSpace(host))
}

// deviceIDFor 把已经拆出来的地址规范化成设备 ID。
func deviceIDFor(host string) string {
	if host == "" {
		return ""
	}
	// 本机自己的地址（回环、以及本机所有网卡地址）统一归到一个 ID。
	// 少了这一步，"用 127.0.0.1 打开"和"用内网 IP 打开"仍是两台设备。
	//
	// 经代理来的地址同样走这一步：代理和浏览器跑在同一台机器上时，
	// 它会看到 127.0.0.1，那本来就该是"本机"。
	if isLocalAddr(host) {
		return localDeviceID
	}
	return normalizeHost(host)
}
