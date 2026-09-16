//go:build windows

// Package desktop 提供 Windows 桌面集成：系统托盘图标、用默认程序打开链接。
//
// 实现只用标准库 + golang.org/x/sys/windows 直接调 Win32，
// 不引入任何第三方 GUI 库，也不需要 cgo——这样交叉编译出来的
// Linux 产物依然是完全静态的 ELF（托盘代码根本不参与编译）。
package desktop

import (
	_ "embed"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed icon.png
var iconPNG []byte

// Options 是托盘图标的行为配置。
type Options struct {
	// Tooltip 是鼠标悬停时显示的提示文字。
	Tooltip string
	// OnOpen 在点击「打开页面」时调用。
	OnOpen func()
	// OnQuit 在点击「退出」时调用。
	OnQuit func()
}

// Supported 报告当前平台是否支持托盘图标。
func Supported() bool { return true }

/* ------------------------------------------------------------ Win32 常量 */

const (
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmContextMenu   = 0x007B
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmApp           = 0x8000
	wmTrayCallback  = wmApp + 1

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	mfString    = 0x00000000
	mfSeparator = 0x00000800

	tpmRightButton = 0x0002
	tpmNoNotify    = 0x0080
	tpmReturnCmd   = 0x0100

	idiApplication = 32512
	lrDefaultColor = 0x00000000

	swShowNormal = 1

	menuOpen = 1
	menuQuit = 2

	// CreateIconFromResourceEx 的 dwVersion，0x00030000 表示资源是 PNG 流
	iconResourceVersion = 0x00030000

	trayIconID = 1
)

/* ------------------------------------------------------------ Win32 结构 */

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type point struct{ x, y int32 }

type msgT struct {
	hwnd    windows.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

// notifyIconDataW 对应 NOTIFYICONDATAW。Windows Vista 及以后接受完整尺寸，
// 所以这里按最新版本声明，cbSize 直接取结构体实际大小。
type notifyIconDataW struct {
	cbSize           uint32
	hWnd             windows.Handle
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            windows.Handle
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         windows.GUID
	hBalloonIcon     windows.Handle
}

/* ------------------------------------------------------------ 动态链接 */

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procTranslateMessage         = user32.NewProc("TranslateMessage")
	procDispatchMessageW         = user32.NewProc("DispatchMessageW")
	procPostQuitMessage          = user32.NewProc("PostQuitMessage")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procCreatePopupMenu          = user32.NewProc("CreatePopupMenu")
	procAppendMenuW              = user32.NewProc("AppendMenuW")
	procTrackPopupMenu           = user32.NewProc("TrackPopupMenu")
	procDestroyMenu              = user32.NewProc("DestroyMenu")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procLoadIconW                = user32.NewProc("LoadIconW")
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon              = user32.NewProc("DestroyIcon")
	procRegisterWindowMessageW   = user32.NewProc("RegisterWindowMessageW")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	procShellExecuteW    = shell32.NewProc("ShellExecuteW")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

/* ------------------------------------------------------------ 托盘状态 */

var (
	trayOpts Options
	// trayHWnd 由托盘线程写、退出路径读，用原子量避免数据竞争
	trayHWnd atomic.Uintptr
	trayIcon windows.Handle
	trayNID  notifyIconDataW

	// Explorer 重启后托盘会被清空，它会广播 TaskbarCreated 通知大家重新添加
	taskbarCreated uint32

	wndProcCallback = syscall.NewCallback(wndProc)

	startOnce sync.Once
	startErr  error
)

// StartTray 在系统托盘放一个图标：左键双击或菜单「打开页面」触发 OnOpen，
// 菜单「退出」触发 OnQuit。函数立即返回，托盘在后台线程里跑消息循环。
//
// 多次调用只有第一次生效。
func StartTray(opts Options) error {
	startOnce.Do(func() {
		startErr = startTray(opts)
	})
	return startErr
}

func startTray(opts Options) error {
	trayOpts = opts

	ready := make(chan error, 1)
	go func() {
		// 窗口的消息循环必须固定在同一个 OS 线程上，否则消息会跑到别的线程去
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		runTray(ready)
	}()

	select {
	case err := <-ready:
		return err
	case <-time.After(5 * time.Second):
		return errors.New("创建托盘图标超时")
	}
}

func runTray(ready chan<- error) {
	hInst, _, _ := procGetModuleHandleW.Call(0)

	name, err := windows.UTF16PtrFromString("QuickShareTrayWindow")
	if err != nil {
		ready <- err
		return
	}

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   wndProcCallback,
		hInstance:     windows.Handle(hInst),
		lpszClassName: name,
	}
	if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		ready <- fmt.Errorf("注册窗口类失败: %v", callErr)
		return
	}

	// 一个永不显示的普通顶层窗口。用 HWND_MESSAGE（消息专用窗口）更"干净"，
	// 但那种窗口 SetForegroundWindow 会失败，右键菜单点外面点不掉。
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(name)),
		0,
		0, // WS_OVERLAPPED，且不调用 ShowWindow，所以不可见
		0, 0, 0, 0,
		0,
		0,
		uintptr(hInst),
		0,
	)
	if hwnd == 0 {
		ready <- fmt.Errorf("创建托盘窗口失败: %v", callErr)
		return
	}
	trayHWnd.Store(uintptr(hwnd))

	if msg, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(mustUTF16("TaskbarCreated")))); msg != 0 {
		taskbarCreated = uint32(msg)
	}

	trayIcon = loadTrayIcon()
	if err := addTrayIcon(); err != nil {
		procDestroyWindow.Call(hwnd)
		ready <- err
		return
	}
	ready <- nil

	var m msgT
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		switch int32(r) {
		case -1: // 出错
			removeTrayIcon()
			return
		case 0: // WM_QUIT
			removeTrayIcon()
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func mustUTF16(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	return p
}

// StopTray 摘掉托盘图标并结束消息循环。进程正常退出前调一次即可。
//
// 不调也不会出错——进程一结束，图标自然消失。但**强制结束**（taskkill / 任务管理器）
// 时来不及清理，通知区域里会留一个点了没反应的"幽灵图标"，要等鼠标划过才消失。
// 走正常退出路径时顺手摘掉，体验干净一些。
func StopTray() {
	if trayHWnd.Load() == 0 {
		return
	}
	removeTrayIcon()
	// 让窗口过程收到 WM_CLOSE → DestroyWindow → WM_DESTROY → PostQuitMessage，
	// 消息循环随即退出
	procPostMessageW.Call(trayHWnd.Load(), wmClose, 0, 0)
}

// iconFromPNG 把内嵌的 PNG 转成图标句柄。Vista 及以后允许直接拿 PNG 当图标资源，
// 省得再去拼 .ico 容器。
func iconFromPNG() windows.Handle {
	if len(iconPNG) == 0 {
		return 0
	}
	h, _, _ := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&iconPNG[0])),
		uintptr(len(iconPNG)),
		1, // fIcon
		iconResourceVersion,
		32, 32,
		lrDefaultColor,
	)
	return windows.Handle(h)
}

// loadTrayIcon 优先用品牌图标，失败就退回系统默认图标，不至于没有图标。
func loadTrayIcon() windows.Handle {
	if h := iconFromPNG(); h != 0 {
		return h
	}
	h, _, _ := procLoadIconW.Call(0, idiApplication)
	return windows.Handle(h)
}

func addTrayIcon() error {
	trayNID = notifyIconDataW{
		cbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		hWnd:             windows.Handle(trayHWnd.Load()),
		uID:              trayIconID,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTrayCallback,
		hIcon:            trayIcon,
	}
	setTip(&trayNID, trayOpts.Tooltip)

	ok, _, _ := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&trayNID)))
	if ok == 0 {
		return errors.New("添加托盘图标失败")
	}
	return nil
}

func removeTrayIcon() {
	if trayHWnd.Load() != 0 {
		procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&trayNID)))
	}
}

func setTip(nid *notifyIconDataW, tip string) {
	if tip == "" {
		tip = "QuickShare"
	}
	u := windows.StringToUTF16(tip)
	// 留一位给结尾的 NUL；超长直接截断
	if len(u) > len(nid.szTip) {
		u = u[:len(nid.szTip)]
		u[len(u)-1] = 0
	}
	copy(nid.szTip[:], u)
}

/* ------------------------------------------------------------ 窗口过程 */

func wndProc(hwnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch {
	case taskbarCreated != 0 && msg == taskbarCreated:
		// Explorer 重启了，把图标重新挂上去
		_ = addTrayIcon()
		return 0

	case msg == wmTrayCallback:
		switch uint32(lParam) & 0xFFFF {
		case wmRButtonUp, wmContextMenu:
			showMenu(hwnd)
		case wmLButtonDblClk:
			invoke(trayOpts.OnOpen)
		}
		return 0

	case msg == wmClose:
		procDestroyWindow.Call(uintptr(hwnd))
		return 0

	case msg == wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}

	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return r
}

func showMenu(hwnd windows.Handle) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	appendMenuItem(menu, menuOpen, "打开页面")
	procAppendMenuW.Call(menu, mfSeparator, 0, 0)
	appendMenuItem(menu, menuQuit, "退出 QuickShare")

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

	// 菜单弹出前要先把窗口设成前台，否则点菜单外面它不会消失
	procSetForegroundWindow.Call(uintptr(hwnd))

	// TPM_RETURNCMD：直接把被点中的菜单项 id 返回，省掉 WM_COMMAND 分发
	cmd, _, _ := procTrackPopupMenu.Call(
		menu,
		tpmRightButton|tpmReturnCmd|tpmNoNotify,
		uintptr(pt.x), uintptr(pt.y),
		0,
		uintptr(hwnd),
		0,
	)

	switch uint32(cmd) {
	case menuOpen:
		invoke(trayOpts.OnOpen)
	case menuQuit:
		invoke(trayOpts.OnQuit)
	}
}

func appendMenuItem(menu uintptr, id uint32, text string) {
	p := mustUTF16(text)
	if p == nil {
		return
	}
	procAppendMenuW.Call(menu, mfString, uintptr(id), uintptr(unsafe.Pointer(p)))
	runtime.KeepAlive(p)
}

// invoke 把回调丢到自己的 goroutine 里执行，避免在窗口过程里做重活——
// 窗口过程阻塞会卡住整个托盘线程的消息循环。
func invoke(fn func()) {
	if fn == nil {
		return
	}
	go fn()
}

/* ------------------------------------------------------------ 打开链接 */

// OpenURL 用系统默认程序打开链接。
func OpenURL(url string) error {
	if url == "" {
		return errors.New("链接为空")
	}
	verb := mustUTF16("open")
	target := mustUTF16(url)
	if target == nil {
		return errors.New("链接不是合法的 UTF-16 字符串")
	}

	// ShellExecuteW 的返回值大于 32 才算成功
	r, _, _ := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(target)),
		0, 0,
		swShowNormal,
	)
	runtime.KeepAlive(target)
	if r <= 32 {
		return fmt.Errorf("打开链接失败（ShellExecute 返回 %d）", r)
	}
	return nil
}
