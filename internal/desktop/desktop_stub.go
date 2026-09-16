//go:build !windows

// Package desktop 在非 Windows 平台上是空实现。
//
// NAS 上的 QuickShare 由 systemd / docker 托管，不需要托盘；
// 而且托盘要依赖桌面环境，在无头服务器上本来也起不来。
// 保持空实现还有个好处：Linux 产物里不会混进任何 GUI 相关代码，
// 交叉编译出来的依然是完全静态的单文件。
package desktop

import "errors"

// ErrUnsupported 表示当前平台不支持该桌面能力。
var ErrUnsupported = errors.New("当前平台不支持该功能（仅 Windows 提供）")

// Options 是托盘图标的行为配置。
type Options struct {
	// Tooltip 是鼠标悬停时显示的提示文字。
	Tooltip string
	// OnOpen 在点击「打开页面」时调用。
	OnOpen func()
	// OnQuit 在点击「退出」时调用。
	OnQuit func()
}

// Supported 报告当前平台是否支持托盘图标。非 Windows 一律返回 false。
func Supported() bool { return false }

// StartTray 在非 Windows 平台上是空操作。
func StartTray(Options) error { return nil }

// StopTray 在非 Windows 平台上是空操作。
func StopTray() {}

// OpenURL 在非 Windows 平台上不做任何事。
func OpenURL(string) error { return ErrUnsupported }
