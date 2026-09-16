//go:build !windows

package desktop

import "testing"

// 非 Windows 平台必须保持空实现：一旦这里返回 true，
// main 就会去调 StartTray，白白给无头服务器添乱。
func TestUnsupportedOnNonWindows(t *testing.T) {
	if Supported() {
		t.Fatal("非 Windows 平台 Supported() 必须为 false")
	}
	if err := StartTray(Options{}); err != nil {
		t.Fatalf("非 Windows 平台 StartTray 应当是空操作，却报错: %v", err)
	}
	if err := OpenURL("http://localhost:8080"); err != ErrUnsupported {
		t.Fatalf("非 Windows 平台 OpenURL 应当返回 ErrUnsupported，实际: %v", err)
	}
}
