//go:build windows

package desktop

import "testing"

func TestSupported(t *testing.T) {
	if !Supported() {
		t.Fatal("Windows 上 Supported() 应当为 true")
	}
}

// TestIconFromPNG 守住内嵌图标这条路：icon.png 必须是 Windows 认得的图标资源。
// 这个断言要是挂了，托盘会悄悄退回系统默认图标——能用，但品牌图标没了。
func TestIconFromPNG(t *testing.T) {
	h := iconFromPNG()
	if h == 0 {
		t.Fatal("icon.png 没能转成图标句柄，托盘会退回系统默认图标")
	}
	procDestroyIcon.Call(uintptr(h))
}

func TestLoadTrayIcon(t *testing.T) {
	if loadTrayIcon() == 0 {
		t.Fatal("loadTrayIcon 连默认图标都没拿到")
	}
}

// TestStartTray 真的把图标挂到托盘上。
// 测试进程退出时图标会随进程消失，不会残留。
func TestStartTray(t *testing.T) {
	if err := StartTray(Options{Tooltip: "QuickShare 测试"}); err != nil {
		t.Fatalf("启动托盘失败: %v", err)
	}
}

// TestOpenURLEmpty 空链接应当直接被拒，不去惊动 ShellExecute。
func TestOpenURLEmpty(t *testing.T) {
	if err := OpenURL(""); err == nil {
		t.Fatal("空链接应当报错")
	}
}
