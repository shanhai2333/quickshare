package main

import (
	"testing"

	"quickshare/internal/server"
	"quickshare/internal/version"
)

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

// updateTarget 决定要不要做更新检查、问哪个源、查哪个仓库。
//
// 这段逻辑看着简单，但**出错是静默的**：源选错、或者该关掉的时候没关掉，表现出来
// 都只是"永远没有新版本提示"，没有任何报错，用户根本不会想到去查环境变量。
// 所以逐条钉住，尤其是下面那条"dockerhub 源没给仓库必须主动关掉"。
func TestUpdateTarget(t *testing.T) {
	// version.Repo 是构建时注入的包级变量，本地 `go build` 是空串。
	// 显式给一个值，才能同时测到"回落到注入值"和"**不**回落"两条路。
	const injected = "owner/injected"
	old := version.Repo
	version.Repo = injected
	t.Cleanup(func() { version.Repo = old })

	cases := []struct {
		name       string
		check      string
		source     string
		repo       string
		wantSource string
		wantRepo   string
	}{
		// ---- 总开关优先 ----
		// 关掉时，哪怕源和仓库都配了也必须什么都不返回
		{"关掉时源和仓库都配了也不检查", "0", "dockerhub", "u/img", "", ""},
		{"关掉时 false 也认", "false", "github", "u/img", "", ""},
		{"关掉时 off 也认", "off", "", "", "", ""},

		// ---- 默认：github 源 ----
		{"默认问 github，仓库取注入值", "", "", "", server.SourceGitHub, injected},
		{"显式写 github 也一样", "1", "github", "", server.SourceGitHub, injected},
		{"源名大小写与空白都认", "1", "  GitHub  ", "", server.SourceGitHub, injected},

		// ---- dockerhub 源 ----
		{"dockerhub 配了仓库就用它", "1", "dockerhub", "shanhaijun/quickshare", server.SourceDockerHub, "shanhaijun/quickshare"},
		{"dockerhub 大小写空白都认", "1", "  DockerHub ", "u/img", server.SourceDockerHub, "u/img"},
		{"仓库两侧空白要裁掉", "1", "dockerhub", "  u/img  ", server.SourceDockerHub, "u/img"},

		// **下面两条是加这个源时最容易写错的地方。**
		// 回落到 version.Repo 的后果是：拿 GitHub 的仓库名去 Docker Hub 查，
		// 必然查不到，而且从外面完全看不出原因——宁可关掉并打日志。
		{"dockerhub 没给仓库必须关掉，不能回落注入值", "1", "dockerhub", "", "", ""},
		{"dockerhub 只给空白也算没给", "1", "dockerhub", "   ", "", ""},

		// ---- 源名认不出来 ----
		{"源名认不出来按 github 处理", "1", "bogus", "", server.SourceGitHub, injected},
		{"认不出来但给了仓库也仍按 github", "1", "bogus", "u/img", server.SourceGitHub, "u/img"},

		// ---- 开关本身写错 ----
		// envBool 对非法值退回默认值（这里是 true），所以检查还是开着。
		// 钉住它，免得哪天把"写错"改成"当关闭"，把用户配好的检查悄悄关掉。
		{"开关写错时退回默认（仍开着）", "maybe", "", "", server.SourceGitHub, injected},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("QS_UPDATE_CHECK", c.check)
			t.Setenv("QS_UPDATE_SOURCE", c.source)
			t.Setenv("QS_UPDATE_REPO", c.repo)

			gotSource, gotRepo := updateTarget()
			if gotSource != c.wantSource || gotRepo != c.wantRepo {
				t.Errorf("updateTarget() = (%q, %q)，期望 (%q, %q)",
					gotSource, gotRepo, c.wantSource, c.wantRepo)
			}
		})
	}
}

// 本地构建（没注入仓库）时不该去问远端。
//
// **注意返回值是 ("github", "")，不是 ("", "")** —— source 仍然返回默认源，
// 只有 repo 为空。我写这条时想当然地以为是两个空串，跑出来才被纠正。
// 两种写法**行为完全一样**：`newUpdateChecker` 里 `repo == ""` 会在发请求之前
// 短路（`update.go` 的 status/fetch 两个入口各有一道），snapshot 也如实给
// `Enabled: false`。所以这里断言的是**真实契约**，别去"修"成两个空串。
//
// 单独一条而不是并进上表：它依赖 version.Repo 为空这个前置，而上表刻意把它设成了
// 非空——两条路的前提正好相反，混在一张表里会看不出在测什么。
func TestUpdateTargetLocalBuildMeansNoCheck(t *testing.T) {
	old := version.Repo
	version.Repo = ""
	t.Cleanup(func() { version.Repo = old })

	t.Setenv("QS_UPDATE_CHECK", "1")
	t.Setenv("QS_UPDATE_SOURCE", "")
	t.Setenv("QS_UPDATE_REPO", "")

	source, repo := updateTarget()
	if source != server.SourceGitHub || repo != "" {
		t.Errorf("没注入仓库时 updateTarget() = (%q, %q)，期望 (%q, \"\")",
			source, repo, server.SourceGitHub)
	}
	// "检查器确实不动"那一半由 internal/server 的 TestUpdateNoRepoMeansNoCheck 盯着
	// （`newUpdateChecker` 是包内私有的，这里调不到，也**不该**为了测试把它导出）。
}
