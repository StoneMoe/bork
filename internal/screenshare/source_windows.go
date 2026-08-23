//go:build windows

package screenshare

import (
	"fmt"
	"runtime/cgo"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	monitorInfoPrimary = 1
	gwlExStyle         = -20
	wsExToolWindow     = 0x80
)

var (
	user32Source = windows.NewLazySystemDLL("user32.dll")

	procEnumDisplayMonitors  = user32Source.NewProc("EnumDisplayMonitors")
	procEnumWindows          = user32Source.NewProc("EnumWindows")
	procGetMonitorInfoW      = user32Source.NewProc("GetMonitorInfoW")
	procGetWindowLongW       = user32Source.NewProc("GetWindowLongW")
	procGetWindowRect        = user32Source.NewProc("GetWindowRect")
	procGetWindowTextLengthW = user32Source.NewProc("GetWindowTextLengthW")
	procGetWindowTextW       = user32Source.NewProc("GetWindowTextW")
	windowExStyleIndex       = int32(gwlExStyle)

	monitorEnumCallback = windows.NewCallback(collectMonitorSource)
	windowEnumCallback  = windows.NewCallback(collectWindowSource)
)

type windowsSourceEnumeration struct {
	monitors []windowsMonitorSource
	windows  []Source
}

type windowsMonitorSource struct {
	handle  uintptr
	rect    windows.Rect
	primary bool
	device  [32]uint16
}

type monitorInfo struct {
	size    uint32
	monitor windows.Rect
	work    windows.Rect
	flags   uint32
	device  [32]uint16
}

func listSources() ([]Source, error) {
	values := &windowsSourceEnumeration{}
	context := cgo.NewHandle(values)
	defer context.Delete()

	// The two groups are independently useful. Some Windows desktops expose
	// displays but no top-level windows, so keep either group that was found.
	procEnumDisplayMonitors.Call(0, 0, monitorEnumCallback, uintptr(context))
	procEnumWindows.Call(windowEnumCallback, uintptr(context))

	sort.Slice(values.monitors, func(i, j int) bool {
		if values.monitors[i].rect.Top != values.monitors[j].rect.Top {
			return values.monitors[i].rect.Top < values.monitors[j].rect.Top
		}
		return values.monitors[i].rect.Left < values.monitors[j].rect.Left
	})
	sources := make([]Source, 0, len(values.monitors)+len(values.windows))
	for _, monitor := range values.monitors {
		name := strings.TrimPrefix(windows.UTF16ToString(monitor.device[:]), `\\.\`)
		if monitor.primary {
			name += "（主显示器）"
		}
		width := int(monitor.rect.Right - monitor.rect.Left)
		height := int(monitor.rect.Bottom - monitor.rect.Top)
		sources = append(sources, Source{
			ID: fmt.Sprintf("%s:%x", SourceMonitor, monitor.handle), Kind: SourceMonitor,
			Name: name, Width: width, Height: height,
		})
	}
	sort.Slice(values.windows, func(i, j int) bool {
		return strings.ToLower(values.windows[i].Name) < strings.ToLower(values.windows[j].Name)
	})
	return append(sources, values.windows...), nil
}

func collectMonitorSource(monitor, _ uintptr, _ *windows.Rect, context uintptr) uintptr {
	values := cgo.Handle(context).Value().(*windowsSourceEnumeration)
	info := monitorInfo{size: uint32(unsafe.Sizeof(monitorInfo{}))}
	result, _, _ := procGetMonitorInfoW.Call(monitor, uintptr(unsafe.Pointer(&info)))
	if result != 0 {
		values.monitors = append(values.monitors, windowsMonitorSource{
			handle: monitor, rect: info.monitor, primary: info.flags&monitorInfoPrimary != 0, device: info.device,
		})
	}
	return 1
}

func collectWindowSource(window, context uintptr) uintptr {
	values := cgo.Handle(context).Value().(*windowsSourceEnumeration)
	if source, ok := windowSource(window); ok {
		values.windows = append(values.windows, source)
	}
	return 1
}

func windowSource(window uintptr) (Source, bool) {
	if !shareableWindow(window) {
		return Source{}, false
	}
	name := windowTitle(window)
	rect, ok := windowFrame(window)
	if name == "" || !ok {
		return Source{}, false
	}
	width := int(rect.Right - rect.Left)
	height := int(rect.Bottom - rect.Top)
	if width < 2 || height < 2 {
		return Source{}, false
	}
	return Source{
		ID: fmt.Sprintf("%s:%x", SourceWindow, window), Kind: SourceWindow,
		Name: name, Width: width, Height: height,
	}, true
}

func shareableWindow(window uintptr) bool {
	if !windows.IsWindowVisible(windows.HWND(window)) {
		return false
	}
	if style, _, _ := procGetWindowLongW.Call(window, uintptr(windowExStyleIndex)); style&wsExToolWindow != 0 {
		return false
	}
	var cloaked uint32
	if windows.DwmGetWindowAttribute(windows.HWND(window), windows.DWMWA_CLOAKED, unsafe.Pointer(&cloaked), uint32(unsafe.Sizeof(cloaked))) == nil && cloaked != 0 {
		return false
	}
	return true
}

func windowTitle(window uintptr) string {
	length, _, _ := procGetWindowTextLengthW.Call(window)
	if length == 0 {
		return ""
	}
	title := make([]uint16, length+1)
	if copied, _, _ := procGetWindowTextW.Call(window, uintptr(unsafe.Pointer(&title[0])), uintptr(len(title))); copied == 0 {
		return ""
	}
	return strings.TrimSpace(windows.UTF16ToString(title))
}

func windowFrame(window uintptr) (windows.Rect, bool) {
	var rect windows.Rect
	if err := windows.DwmGetWindowAttribute(windows.HWND(window), windows.DWMWA_EXTENDED_FRAME_BOUNDS, unsafe.Pointer(&rect), uint32(unsafe.Sizeof(rect))); err != nil {
		if result, _, _ := procGetWindowRect.Call(window, uintptr(unsafe.Pointer(&rect))); result == 0 {
			return windows.Rect{}, false
		}
	}
	return rect, true
}
