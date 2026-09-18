package server

import (
	"net/http"
	"testing"
)

// ---------------------------------------------------------------- 名单解析

func TestParseTrustedProxies(t *testing.T) {
	// 单个 IP、网段、IPv6 混着写，中间的空格和空项都要能容忍
	p := parseTrustedProxies(" 10.0.0.5 , 172.16.0.0/12 ,::1, 2001:db8::/32 ,")
	if len(p) != 4 {
		t.Fatalf("解析出 %d 项，期望 4 项", len(p))
	}

	yes := []string{"10.0.0.5", "172.16.3.9", "::1", "2001:db8::99"}
	for _, in := range yes {
		if !p.contains(in) {
			t.Errorf("%s 在名单里，contains 却说不在", in)
		}
	}
	no := []string{"10.0.0.6", "172.32.0.1", "192.168.1.1", "2001:db9::1", "::2", ""}
	for _, in := range no {
		if p.contains(in) {
			t.Errorf("%s 不在名单里，contains 却说在", in)
		}
	}

	// 单个 IP 必须当成"只有它自己"，不能因为掩码算错把整个网段都放进来。
	if parseTrustedProxies("10.0.0.5").contains("10.0.0.6") {
		t.Error("单个 IP 被当成了网段：10.0.0.6 不该被信任")
	}
}

func TestParseTrustedProxiesSkipsGarbage(t *testing.T) {
	// 认不出来的项跳过、不 panic，也不影响同一串里合法的那些。
	// 配置写错时退化成"不信任代理"（老行为），比整个服务起不来好。
	p := parseTrustedProxies("10.0.0.5, 不是IP, 10.0.0.0/99, ,192.168.1.1")
	if len(p) != 2 {
		t.Fatalf("解析出 %d 项，期望 2 项（只有两项合法）", len(p))
	}
	if !p.contains("10.0.0.5") || !p.contains("192.168.1.1") {
		t.Error("合法的两项应当都在名单里")
	}

	// 空串是默认值，必须是"一个都不信"
	if n := len(parseTrustedProxies("")); n != 0 {
		t.Errorf("空配置解析出 %d 项，期望 0 项", n)
	}
	if n := len(parseTrustedProxies("   ")); n != 0 {
		t.Errorf("全空白的配置解析出 %d 项，期望 0 项", n)
	}
}

// ---------------------------------------------------------------- 从代理头取客户端

// 这一组是这个功能的安全边界。**第一条（对端不受信时一个头都不读）是重点**：
// 少了它，任何人都能编一个 X-Forwarded-For 去冒充别的设备。
func TestRealClientOnlyTrustsConfiguredProxies(t *testing.T) {
	p := parseTrustedProxies("10.0.0.5, 172.16.0.0/12")

	cases := []struct {
		name   string
		remote string
		xff    string
		realIP string
		want   string
	}{
		{
			name:   "对端不是受信代理 → 完全不理这些头",
			remote: "203.0.113.9:5555",
			xff:    "198.51.100.7",
			want:   "",
		},
		{
			name:   "对端不是受信代理 → X-Real-IP 同样不理",
			remote: "203.0.113.9:5555",
			realIP: "198.51.100.7",
			want:   "",
		},
		{
			name:   "对端是受信代理 → 取 XFF 里的地址",
			remote: "10.0.0.5:5555",
			xff:    "198.51.100.7",
			want:   "198.51.100.7",
		},
		{
			// **核心断言**：代理会把真实地址追加在右边，客户端自己塞的留在左边。
			// 从右往左取，左边那个伪造的 127.0.0.1 必须被忽略。
			name:   "客户端伪造的左侧条目被忽略（从右往左取）",
			remote: "10.0.0.5:5555",
			xff:    "127.0.0.1, 198.51.100.7",
			want:   "198.51.100.7",
		},
		{
			name:   "链上有多个代理，跳过受信的那些继续往左",
			remote: "10.0.0.5:5555",
			xff:    "198.51.100.7, 172.16.9.9, 10.0.0.5",
			want:   "198.51.100.7",
		},
		{
			name:   "网段也认（172.16/12 内的地址算受信代理）",
			remote: "172.20.1.1:5555",
			xff:    "198.51.100.7, 172.20.1.1",
			want:   "198.51.100.7",
		},
		{
			name:   "IPv6 客户端",
			remote: "10.0.0.5:5555",
			xff:    "2001:db8::5",
			want:   "2001:db8::5",
		},
		{
			// 代理可能写 "unknown" 这种非 IP 的东西，不能拿它当设备 ID
			name:   "XFF 全是非 IP → 不采信，返回空串",
			remote: "10.0.0.5:5555",
			xff:    "unknown",
			want:   "",
		},
		{
			name:   "XFF 是垃圾但 X-Real-IP 可用 → 退回 X-Real-IP",
			remote: "10.0.0.5:5555",
			xff:    "unknown",
			realIP: "198.51.100.7",
			want:   "198.51.100.7",
		},
		{
			name:   "整条链都是受信代理 → 不硬猜，看 X-Real-IP",
			remote: "10.0.0.5:5555",
			xff:    "172.16.9.9, 10.0.0.5",
			realIP: "198.51.100.7",
			want:   "198.51.100.7",
		},
		{
			name:   "没有 XFF 时看 X-Real-IP",
			remote: "10.0.0.5:5555",
			realIP: "198.51.100.7",
			want:   "198.51.100.7",
		},
		{
			name:   "两个头都没有 → 空串（调用方退回源地址）",
			remote: "10.0.0.5:5555",
			want:   "",
		},
	}

	for _, c := range cases {
		h := http.Header{}
		if c.xff != "" {
			h.Set("X-Forwarded-For", c.xff)
		}
		if c.realIP != "" {
			h.Set("X-Real-IP", c.realIP)
		}
		if got := p.realClient(c.remote, h); got != c.want {
			t.Errorf("%s：realClient(%q, xff=%q, xri=%q) = %q，期望 %q",
				c.name, c.remote, c.xff, c.realIP, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- 接进 clientIP

func proxyTestServer(trusted string) *Server {
	return New(Config{DataDir: "/tmp/qs-proxy-test", TrustedProxies: trusted}, nil, textTestWeb())
}

// 没配名单时，X-Forwarded-For 必须被彻底无视——这是默认行为，也是升级后
// 唯一安全的行为。以前就是这个行为，加了功能不能把它改掉。
func TestClientIPIgnoresForwardedByDefault(t *testing.T) {
	s := proxyTestServer("")
	r := &http.Request{
		RemoteAddr: "203.0.113.9:5555",
		Header:     http.Header{"X-Forwarded-For": []string{"198.51.100.7"}},
	}
	if got := s.clientIP(r); got != "203.0.113.9" {
		t.Errorf("没配受信代理时 clientIP = %q，期望源地址 203.0.113.9", got)
	}
}

// 配了名单、且对端就在名单里，才认 X-Forwarded-For。
func TestClientIPUsesForwardedWhenTrusted(t *testing.T) {
	s := proxyTestServer("10.0.0.5")
	r := &http.Request{
		RemoteAddr: "10.0.0.5:5555",
		Header:     http.Header{"X-Forwarded-For": []string{"198.51.100.7"}},
	}
	if got := s.clientIP(r); got != "198.51.100.7" {
		t.Errorf("受信代理转发的 clientIP = %q，期望 198.51.100.7", got)
	}
}

// 对端不在名单里 → 编一个 XFF 也没用，身份仍是它自己的地址。
func TestClientIPIgnoresForwardedFromUntrusted(t *testing.T) {
	s := proxyTestServer("10.0.0.5")
	r := &http.Request{
		RemoteAddr: "203.0.113.9:5555",
		Header:     http.Header{"X-Forwarded-For": []string{"127.0.0.1"}},
	}
	if got := s.clientIP(r); got != "203.0.113.9" {
		t.Errorf("不受信来源的 clientIP = %q，期望它自己的地址 203.0.113.9", got)
	}
}

// 经代理来的"本机"地址仍然要归一。
//
// 代理和浏览器跑在同一台机器上时（NAS 上自用就是这个场景），代理看到的是
// 127.0.0.1——那本来就该算"本机"。少了这一步，同一台电脑会多出一台设备。
func TestClientIPNormalizesForwardedLocalAddr(t *testing.T) {
	s := proxyTestServer("127.0.0.1")
	r := &http.Request{
		RemoteAddr: "127.0.0.1:5555",
		Header:     http.Header{"X-Forwarded-For": []string{"127.0.0.1"}},
	}
	if got := s.clientIP(r); got != localDeviceID {
		t.Errorf("经代理来的回环地址 clientIP = %q，期望归一为 %q", got, localDeviceID)
	}
}
