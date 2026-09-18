package server

import (
	"strings"
	"testing"
	"time"

	"quickshare/internal/store"
)

// ---------------------------------------------------------------- 验证码识别

func TestLooksLikeCode(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		// 第一档：整条就是 4~8 位数字
		{"六位数字", "123456", true},
		{"四位数字", "1234", true},
		{"八位数字", "12345678", true},
		{"首尾空白不算", "  123456\n", true},
		{"三位太短", "123", false},
		{"九位太长", "123456789", false},
		{"数字里混了字母", "12a456", false},

		// 第二档：关键词 + 至少 4 位连续数字
		{"短信原文", "【某某】验证码 123456，5 分钟内有效", true},
		{"中文冒号", "验证码：8642", true},
		{"英文关键词", "Your verification code is 987654", true},
		{"otp", "OTP 4455", true},
		{"关键词但没数字", "验证码已发送", false},
		{"关键词但数字不够长", "code 12", false},
		{"没有任何线索", "今天花了 1234 元", false},
		{"普通一句话", "晚上七点老地方见", false},

		// 长度上限：长文本即使含关键词和数字也不算——防的是把一段源码
		// 或报错日志当验证码，10 分钟后悄悄删掉
		{"超长文本带 code 和数字", strings.Repeat("x", codeTextMaxLen) + " code 1234", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeCode(c.content); got != c.want {
				t.Fatalf("looksLikeCode(%q) = %v，期望 %v", c.content, got, c.want)
			}
		})
	}
}

// 长度上限的边界：正好等于上限时仍然算，超一个字符就不算。
func TestLooksLikeCodeLengthBoundary(t *testing.T) {
	// 把关键词和数字放在最前面，剩下的用 x 补到指定长度
	build := func(n int) string {
		head := "code 1234"
		return head + strings.Repeat("x", n-len(head))
	}
	if !looksLikeCode(build(codeTextMaxLen)) {
		t.Fatalf("正好 %d 字符时应当算验证码", codeTextMaxLen)
	}
	if looksLikeCode(build(codeTextMaxLen + 1)) {
		t.Fatalf("超过 %d 字符时不该算验证码", codeTextMaxLen)
	}
}

func TestHasDigitRun(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want bool
	}{
		{"abc1234", 4, true},
		{"abc123", 4, false},
		{"1234abc", 4, true},
		{"1a2b3c4d", 4, false}, // 连续才算
		{"", 1, false},
	}
	for _, c := range cases {
		if got := hasDigitRun(c.in, c.n); got != c.want {
			t.Fatalf("hasDigitRun(%q, %d) = %v，期望 %v", c.in, c.n, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- 单位换算

func TestUnitDuration(t *testing.T) {
	cases := []struct {
		n        int
		unit     string
		defUnit  string
		want     time.Duration
		wantOK   bool
		describe string
	}{
		{30, "minute", "day", 30 * time.Minute, true, "分钟"},
		{2, "hour", "day", 2 * time.Hour, true, "小时"},
		{3, "day", "minute", 72 * time.Hour, true, "天"},
		{5, "", "day", 120 * time.Hour, true, "单位为空时用默认单位"},
		{5, "week", "day", 0, false, "认不出来的单位"},
	}
	for _, c := range cases {
		got, ok := unitDuration(c.n, c.unit, c.defUnit)
		if ok != c.wantOK || got != c.want {
			t.Fatalf("%s: unitDuration(%d, %q, %q) = (%v, %v)，期望 (%v, %v)",
				c.describe, c.n, c.unit, c.defUnit, got, ok, c.want, c.wantOK)
		}
	}
}

func TestTTLFromSettings(t *testing.T) {
	// 文本：没设 = 不启用
	if d, ok := textTTLFrom(map[string]string{}); ok || d != 0 {
		t.Fatalf("没设文本保留时长时应当是未启用，实际 (%v, %v)", d, ok)
	}
	kv := map[string]string{
		store.SettingTextTTLValue: "24",
		store.SettingTextTTLUnit:  "hour",
	}
	if d, ok := textTTLFrom(kv); !ok || d != 24*time.Hour {
		t.Fatalf("文本保留时长 = (%v, %v)，期望 24h", d, ok)
	}

	// 文件同理
	if d, ok := fileTTLFrom(map[string]string{}); ok || d != 0 {
		t.Fatalf("没设文件保留时长时应当是未启用，实际 (%v, %v)", d, ok)
	}

	// 验证码：**没设过**时给默认值 10 分钟，跟另外两档不同
	if d, ok := codeTTLFrom(map[string]string{}); !ok || d != defaultCodeTTL {
		t.Fatalf("没设验证码保留时长时应当是默认 %v，实际 (%v, %v)", defaultCodeTTL, d, ok)
	}
	if d, ok := codeTTLFrom(map[string]string{store.SettingCodeTTLValue: ""}); !ok || d != defaultCodeTTL {
		t.Fatalf("键存在但为空时应当仍是默认值，实际 (%v, %v)", d, ok)
	}
	// 显式 0 = 关掉这条规则（不是"永不删除"，也不是"用默认值"）
	if d, ok := codeTTLFrom(map[string]string{store.SettingCodeTTLValue: "0"}); ok || d != 0 {
		t.Fatalf("显式设成 0 时应当是关闭，实际 (%v, %v)", d, ok)
	}
	// 认不出来的值也当关闭——往"少删"的方向偏
	if d, ok := codeTTLFrom(map[string]string{store.SettingCodeTTLValue: "abc"}); ok || d != 0 {
		t.Fatalf("值解析不了时应当是关闭，实际 (%v, %v)", d, ok)
	}
	// 设了数值没设单位时用分钟（这档的默认单位）
	if d, ok := codeTTLFrom(map[string]string{store.SettingCodeTTLValue: "30"}); !ok || d != 30*time.Minute {
		t.Fatalf("只填数值时应当按分钟算，实际 (%v, %v)", d, ok)
	}
}

// ---------------------------------------------------------------- 优先级

// 到期时刻的优先级：单条覆盖 > 验证码规则 > 全局。
func TestPolicyExpiry(t *testing.T) {
	created := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	p := ttlPolicy{
		text: 24 * time.Hour,
		code: 10 * time.Minute,
		file: 48 * time.Hour,
	}

	// 普通文本走全局
	plain := &store.Text{CreatedAt: created}
	if got, want := p.textExpiry(plain), created.Add(24*time.Hour).Unix(); got != want {
		t.Fatalf("普通文本到期时刻 = %d，期望 %d", got, want)
	}

	// 验证码走验证码那档
	code := &store.Text{CreatedAt: created, IsCode: true}
	if got, want := p.textExpiry(code), created.Add(10*time.Minute).Unix(); got != want {
		t.Fatalf("验证码到期时刻 = %d，期望 %d", got, want)
	}

	// 验证码但自己设了时长 → 单条覆盖压过验证码规则
	own := &store.Text{CreatedAt: created, IsCode: true, TTLSeconds: 7200}
	if got, want := p.textExpiry(own), created.Add(2*time.Hour).Unix(); got != want {
		t.Fatalf("单条覆盖后的到期时刻 = %d，期望 %d", got, want)
	}

	// 验证码那档被关掉时，验证码按普通文本走
	off := ttlPolicy{text: 24 * time.Hour}
	if got, want := off.textExpiry(code), created.Add(24*time.Hour).Unix(); got != want {
		t.Fatalf("验证码规则关闭时的到期时刻 = %d，期望 %d", got, want)
	}

	// 两档都没启用 = 永不删除（0）
	none := ttlPolicy{}
	if got := none.textExpiry(code); got != 0 {
		t.Fatalf("两档都没启用时应当返回 0（永不删除），实际 %d", got)
	}
	if got := none.fileExpiry(&store.File{CreatedAt: created}); got != 0 {
		t.Fatalf("文件两档都没启用时应当返回 0，实际 %d", got)
	}

	// 文件：全局那档
	f := &store.File{CreatedAt: created}
	if got, want := p.fileExpiry(f), created.Add(48*time.Hour).Unix(); got != want {
		t.Fatalf("文件到期时刻 = %d，期望 %d", got, want)
	}
	// 文件：单条覆盖
	f.TTLSeconds = 600
	if got, want := p.fileExpiry(f), created.Add(10*time.Minute).Unix(); got != want {
		t.Fatalf("文件单条覆盖后的到期时刻 = %d，期望 %d", got, want)
	}
}
