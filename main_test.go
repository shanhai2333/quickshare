package main

import "testing"

// localURLOf 决定"本机访问"的地址，托盘菜单的「打开页面」用的也是它。
// 重点是**绑定了具体地址时不能退回 localhost**——那种情况下 localhost 上没有监听。
func TestLocalURLOf(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		// 通配地址：绑在所有接口上，localhost 能通
		{":8080", "http://localhost:8080"},
		{"0.0.0.0:8080", "http://localhost:8080"},
		{"[::]:8080", "http://localhost:8080"},
		// 具体地址：只能用这个地址访问
		{"127.0.0.1:8090", "http://127.0.0.1:8090"},
		{"192.168.1.5:8080", "http://192.168.1.5:8080"},
		{"[::1]:8080", "http://[::1]:8080"},
		// 不是 host:port 形式，按"本机 + 端口"兜底
		{"8080", "http://localhost:8080"},
	}
	for _, c := range cases {
		if got := localURLOf(c.addr); got != c.want {
			t.Errorf("localURLOf(%q) = %q，期望 %q", c.addr, got, c.want)
		}
	}
}

func TestPortOf(t *testing.T) {
	cases := []struct{ addr, want string }{
		{":8080", "8080"},
		{"127.0.0.1:18080", "18080"},
		{"[::1]:9000", "9000"},
		{"8080", "8080"}, // 取不出来就原样返回
	}
	for _, c := range cases {
		if got := portOf(c.addr); got != c.want {
			t.Errorf("portOf(%q) = %q，期望 %q", c.addr, got, c.want)
		}
	}
}

// envBool 是 QS_TRAY 这类开关的解析入口，边界比看上去多：
// 大小写、两侧空白、以及"写了但写错"时必须安静地退回默认值，而不是崩掉。
func TestEnvBool(t *testing.T) {
	const key = "QS_TEST_BOOL"
	cases := []struct {
		val  string
		def  bool
		want bool
	}{
		{"", true, true},   // 没设置 → 默认
		{"", false, false}, //
		{"1", false, true},
		{"true", false, true},
		{"YES", false, true},    // 大小写不敏感
		{"  On  ", false, true}, // 两侧空白也要认
		{"0", true, false},
		{"false", true, false},
		{"off", true, false},
		{"maybe", true, true},   // 非法值 → 默认（并往 stderr 提一句）
		{"maybe", false, false}, //
	}
	for _, c := range cases {
		t.Setenv(key, c.val)
		if got := envBool(key, c.def); got != c.want {
			t.Errorf("envBool(%q=%q, 默认 %v) = %v，期望 %v", key, c.val, c.def, got, c.want)
		}
	}
}
