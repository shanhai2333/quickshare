// Package version 记录构建时注入的版本信息，并提供语义化版本比较。
//
// 这四个变量是**构建时**用 `-ldflags -X` 注入的（见 Makefile、Dockerfile、
// .github/workflows/release.yml）。源码里保持默认值，于是 `go build` 直接编出来的
// 东西老老实实叫自己"开发版"，而不是假装成某个正式版本——版本号要是能撒谎，
// 用户报问题时报的版本就不可信了。
package version

import "strings"

// DevVersion 是没注入版本号时的占位值。
const DevVersion = "dev"

var (
	// Version 是语义化版本号。**git tag 上的 v 前缀在注入前会剥掉**，
	// 内部统一不带 v（比较函数两种写法都认，但只存一种，免得到处判断）。
	Version = DevVersion

	// Commit 是构建所用的短 commit hash。
	Commit = ""

	// Date 是构建时间，RFC3339（UTC）。
	Date = ""

	// Repo 是 "owner/name" 形式的 GitHub 仓库，更新检查用。
	//
	// CI 里注入 ${{ github.repository }}。**本地构建留空** —— 空表示"不做更新检查"，
	// 这正好是开发时想要的：自己编的二进制不该去问远端有没有新版本。
	Repo = ""
)

// IsRelease 报告这是不是一个正式发布的构建。
func IsRelease() bool { return Version != "" && Version != DevVersion }

// String 返回给日志和界面看的一行版本串。
func String() string {
	var b strings.Builder
	b.WriteString(Version)
	if Commit != "" {
		b.WriteString(" (")
		b.WriteString(Commit)
		b.WriteByte(')')
	}
	if Date != "" {
		b.WriteByte(' ')
		b.WriteString(Date)
	}
	return b.String()
}

// Compare 按语义化版本比较 a 和 b：a < b 返回 -1，相等返回 0，a > b 返回 1。
//
// **认不出来的一律当相等（返回 0）**，这一点是刻意的：更新检查里"比不出来"的
// 正确表现是"不提示有更新"，而不是误报。GitHub 上的 tag 千奇百怪，宁可漏报
// 不可误报——天天弹一个假的"有新版本"比不弹更烦人。
//
// 预发布后缀（`-rc1` 之类）按字典序比，没有后缀的正式版大于有后缀的。
// 严格按 semver 规范，数字标识符应该按数值比（rc2 < rc10），这里简化成字典序：
// 对本项目的用途（判断"要不要提示升级"）够了，不值得为它引一个依赖。
func Compare(a, b string) int {
	pa, oka := parse(a)
	pb, okb := parse(b)
	if !oka || !okb {
		return 0
	}
	for _, d := range [3][2]int{
		{pa.major, pb.major},
		{pa.minor, pb.minor},
		{pa.patch, pb.patch},
	} {
		if c := cmpInt(d[0], d[1]); c != 0 {
			return c
		}
	}
	switch {
	case pa.pre == pb.pre:
		return 0
	case pa.pre == "":
		return 1 // 正式版 > 预发布版
	case pb.pre == "":
		return -1
	}
	return strings.Compare(pa.pre, pb.pre)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

type semver struct {
	major, minor, patch int
	// parts 记录**原样写了几段**（1 / 2 / 3）。比较用不到它，但 IsFullSemver
	// 要——`1.2` 和 `1.2.0` 解析出来的三个数字完全一样，只有段数能区分。
	parts int
	pre   string
}

// parse 解析 `1.2.3` / `v1.2.3` / `1.2` / `1` / `1.2.3-rc1+build5`。
//
// 允许缺省小版本号和补丁号：git tag 里 `v1` / `v1.2` 并不少见。
func parse(s string) (semver, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if s == "" {
		return semver{}, false
	}
	// 去掉 build metadata：按规范它不参与比较
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i+1:]
		if pre == "" {
			// "1.0.0-" 不是合法版本号。**必须显式拒绝**：留空的话它和
			// "1.0.0" 解析出来一模一样，会被当成"正式版"，比较结果全错。
			return semver{}, false
		}
		s = s[:i]
	}

	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return semver{}, false
	}
	var out semver
	fields := [3]*int{&out.major, &out.minor, &out.patch}
	for i, p := range parts {
		n, ok := atoiStrict(p)
		if !ok {
			return semver{}, false
		}
		*fields[i] = n
	}
	out.parts = len(parts)
	out.pre = pre
	return out, true
}

// IsFullSemver 报告 s 是不是**写全了三段**的版本号（`1.2.3`，可带 `-rc1` 这类后缀）。
//
// 这是给 Docker Hub 挑最新 tag 用的：那边没有"直接告诉我最新版"的接口，只给一个
// tag 列表，得自己算最大。而那个列表里混着 `latest`、`1.0`，以及日期式标签
// （`20260916` 会被解析成 major=20260916，看着比谁都大）。**只认三段式能把它们
// 挡在外面。**
//
// 刻意不认 `1.2` 这种省略写法：本项目的 `docker/metadata-action` 固定产出
// `1.0.0` / `1.0` / `latest` 三个标签，三段式那个一定在，所以不认它不会漏。
// **宁可漏报不可误报**——认错了会弹一个假的"有新版本"。
func IsFullSemver(s string) bool {
	sv, ok := parse(s)
	return ok && sv.parts == 3
}

// atoiStrict 只接受纯数字。刻意不用 strconv.Atoi —— 它认 "+1" 和 "-1"，
// 而 "-1" 会让 `1.-1.0` 这种明显不是版本号的东西混进来。
func atoiStrict(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return 0, false // 溢出了，肯定不是版本号
		}
	}
	return n, true
}
