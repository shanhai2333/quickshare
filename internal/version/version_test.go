package version

import "testing"

// Compare 是"要不要提示升级"的唯一判据，判错的表现是**天天弹一个假的更新提示**
// （或者真更新了却不弹），所以边界要一条条钉住。
func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// 基本的三段比较
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.1.0", -1},
		{"1.9.0", "1.10.0", -1}, // 不能按字符串比（"9" > "1"）
		{"2.0.0", "1.99.99", 1},

		// v 前缀两种写法都要认
		{"v1.2.3", "1.2.3", 0},
		{"1.2.3", "v1.2.4", -1},
		{"V1.2.3", "v1.2.3", 0},

		// 缺省的小版本号 / 补丁号
		{"1", "1.0.0", 0},
		{"1.2", "1.2.0", 0},
		{"1.2", "1.2.1", -1},
		{"v2", "v1.9.9", 1},

		// 预发布：正式版更大
		{"1.0.0-rc1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc1", 1},
		{"1.0.0-rc1", "1.0.0-rc1", 0},
		{"1.0.0-rc1", "1.0.0-rc2", -1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
		// 预发布版之间比不出大小也不该崩，更不能把数字段吃掉
		{"1.1.0-rc1", "1.0.0", 1},

		// build metadata 不参与比较
		{"1.0.0+build5", "1.0.0", 0},
		{"1.0.0+aaa", "1.0.0+bbb", 0},

		// 认不出来的一律当相等 —— 宁可漏报不可误报。
		// 这几条是**故意**的：本地 `git describe --dirty` 会产出这种东西，
		// 拿它去比只会得到一堆假的"有新版本"。
		{"dev", "1.2.3", 0},
		{"1.2.3", "dev", 0},
		{"", "1.2.3", 0},
		{"1.2.3", "", 0},
		{"abc", "abd", 0},
		{"1.2.3.4", "1.2.3", 0},
		{"1.2b", "1.2.3", 0},
		{"-1.0.0", "0.0.0", 0},
		{"+1.0.0", "0.0.0", 0},
		{"1..0", "1.0.0", 0},
		{"1.0.0-", "1.0.0", 0}, // 空后缀不是合法版本号。**不能**当成"空串预发布版"
		{"1.0.0-", "1.0.0-rc1", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d，期望 %d", c.a, c.b, got, c.want)
		}
	}
}

// Compare 必须满足反对称性，否则"a 比 b 新"和"b 比 a 新"可能同时成立。
func TestCompareAntisymmetric(t *testing.T) {
	versions := []string{"dev", "1.0.0", "v1.2.3", "1.2", "2.0.0-rc1", "2.0.0", "", "abc"}
	for _, a := range versions {
		for _, b := range versions {
			if x, y := Compare(a, b), Compare(b, a); x != -y {
				t.Errorf("Compare(%q,%q)=%d 但 Compare(%q,%q)=%d，不满足反对称", a, b, x, b, a, y)
			}
		}
	}
}

func TestIsRelease(t *testing.T) {
	orig := Version
	defer func() { Version = orig }()

	Version = DevVersion
	if IsRelease() {
		t.Error("dev 不该被当成正式发布")
	}
	Version = ""
	if IsRelease() {
		t.Error("空版本号不该被当成正式发布")
	}
	Version = "1.2.3"
	if !IsRelease() {
		t.Error("1.2.3 应当被当成正式发布")
	}
}

func TestString(t *testing.T) {
	origV, origC, origD := Version, Commit, Date
	defer func() { Version, Commit, Date = origV, origC, origD }()

	Version, Commit, Date = DevVersion, "", ""
	if got := String(); got != "dev" {
		t.Errorf("String() = %q，期望 %q", got, "dev")
	}

	Version, Commit, Date = "1.2.3", "abc1234", "2026-09-16T00:00:00Z"
	if got, want := String(), "1.2.3 (abc1234) 2026-09-16T00:00:00Z"; got != want {
		t.Errorf("String() = %q，期望 %q", got, want)
	}
}
