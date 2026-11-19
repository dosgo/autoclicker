package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

//go:embed static
var staticFS embed.FS

type WNDCLASSEXW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

// ---------- Windows API (user32) ----------

var (
	user32              = syscall.NewLazyDLL("user32.dll")
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	gdi32               = syscall.NewLazyDLL("gdi32.dll")
	procSetCursorPos    = user32.NewProc("SetCursorPos")
	procGetCursorPos    = user32.NewProc("GetCursorPos")
	procMouseEvent      = user32.NewProc("mouse_event")
	procGetAsyncKeyState = user32.NewProc("GetAsyncKeyState")
	procRegisterHotKey  = user32.NewProc("RegisterHotKey")
	procGetMessageW     = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
	procSetTimer        = user32.NewProc("SetTimer")
	procSetWindowTextW  = user32.NewProc("SetWindowTextW")
	procSendMessageW    = user32.NewProc("SendMessageW")
	procGetStockObject  = gdi32.NewProc("GetStockObject")
	procSetTextColor    = gdi32.NewProc("SetTextColor")
	procSetBkColor      = gdi32.NewProc("SetBkColor")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
)

type pt struct {
	X, Y int32
}

const (
	mLEFTDOWN  = 0x0002
	mLEFTUP    = 0x0004
	mRIGHTDOWN = 0x0008
	mRIGHTUP   = 0x0010
)

func setCursor(x, y int) {
	procSetCursorPos.Call(uintptr(int32(x)), uintptr(int32(y)))
}

func getCursor() (int, int) {
	var p pt
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	return int(p.X), int(p.Y)
}

func rawClick(down, up uintptr) {
	procMouseEvent.Call(down, 0, 0, 0, 0)
	time.Sleep(40 * time.Millisecond)
	procMouseEvent.Call(up, 0, 0, 0, 0)
}

func doClick(kind string) {
	switch kind {
	case "right":
		rawClick(mRIGHTDOWN, mRIGHTUP)
	case "double":
		rawClick(mLEFTDOWN, mLEFTUP)
		time.Sleep(60 * time.Millisecond)
		rawClick(mLEFTDOWN, mLEFTUP)
	default: // left
		rawClick(mLEFTDOWN, mLEFTUP)
	}
}

func escPressed() bool {
	r, _, _ := procGetAsyncKeyState.Call(0x1B) // VK_ESCAPE
	return r&0x8000 != 0
}

// ---------- 实时坐标悬浮窗（置顶、可拖动、不进任务栏） ----------

const (
	WS_POPUP   = 0x80000000
	WS_CHILD   = 0x40000000
	WS_VISIBLE = 0x10000000
	WS_EX_TOPMOST    = 0x00000008
	WS_EX_TOOLWINDOW = 0x00000080
	WS_EX_LAYERED    = 0x00080000
	WM_CREATE        = 0x0001
	WM_TIMER         = 0x0113
	WM_NCHITTEST     = 0x0084
	WM_CTLCOLORSTATIC = 0x0138
	WM_SETFONT       = 0x0030
	HTCAPTION        = 2
)

var (
	overlayStatic  uintptr
	bgBrush        uintptr
	hInstGlobal    uintptr
	staticClassPtr *uint16
)

// 颜色: COLORREF = 0x00BBGGRR
const overlayBG = 0x221610 // 深暗蓝背景 R16 G22 B34
const overlayFG = 0x8CFF50 // 亮绿文字 R80 G255 B140

func overlayWndProc(hwnd, m, wp, lp uintptr) uintptr {
	switch m {
	case WM_CREATE:
		text, _ := syscall.UTF16PtrFromString("X:0  Y:0")
		overlayStatic, _, _ = procCreateWindowExW.Call(0,
			uintptr(unsafe.Pointer(staticClassPtr)),
			uintptr(unsafe.Pointer(text)),
			uintptr(WS_CHILD|WS_VISIBLE),
			4, 4, 150, 28, hwnd, 0, hInstGlobal, 0)
		font, _, _ := procGetStockObject.Call(17) // DEFAULT_GUI_FONT
		procSendMessageW.Call(overlayStatic, WM_SETFONT, font, 1)
		return 0
	case WM_TIMER: // 每 100ms 刷新一次坐标
		x, y := getCursor()
		s, _ := syscall.UTF16PtrFromString(fmt.Sprintf("X:%d  Y:%d", x, y))
		procSetWindowTextW.Call(overlayStatic, uintptr(unsafe.Pointer(s)))
		return 0
	case WM_CTLCOLORSTATIC: // 设置文字/背景颜色
		procSetTextColor.Call(wp, overlayFG)
		procSetBkColor.Call(wp, overlayBG)
		return bgBrush
	case WM_NCHITTEST: // 整个窗口当标题栏 → 按住可拖动
		return HTCAPTION
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, m, wp, lp)
	return r
}

func overlayLoop() {
	runtime.LockOSThread()
	hInst, _, _ := procGetModuleHandleW.Call(0)
	hInstGlobal = hInst
	className, _ := syscall.UTF16PtrFromString("AutoClickerOverlay")
	cls := WNDCLASSEXW{
		CbSize:        uint32(unsafe.Sizeof(WNDCLASSEXW{})),
		LpfnWndProc:   syscall.NewCallback(overlayWndProc),
		HInstance:     hInst,
		LpszClassName: className,
	}
	atom, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&cls)))
	if atom == 0 {
		fmt.Println("警告: 悬浮窗注册失败")
		return
	}
	bgBrush, _, _ = procCreateSolidBrush.Call(overlayBG)
	staticClass, _ := syscall.UTF16PtrFromString("STATIC")
	staticClassPtr = staticClass
	sw, _, _ := procGetSystemMetrics.Call(0) // SM_CXSCREEN
	title, _ := syscall.UTF16PtrFromString("")
	hwnd, _, _ := procCreateWindowExW.Call(
		uintptr(WS_EX_TOPMOST|WS_EX_TOOLWINDOW|WS_EX_LAYERED),
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		uintptr(WS_POPUP|WS_VISIBLE),
		uintptr(int(sw)-175), 8, 160, 38,
		0, 0, hInst, 0)
	if hwnd == 0 {
		fmt.Println("警告: 悬浮窗创建失败")
		return
	}
	procSetLayeredWindowAttributes.Call(hwnd, 0, 235, 2) // 半透明
	procSetTimer.Call(hwnd, 1, 100, 0)
	var m winmsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// ---------- F8 全局热键捕获坐标 ----------

type Capture struct {
	X  int   `json:"x"`
	Y  int   `json:"y"`
	Ts int64 `json:"ts"` // unix 毫秒，0 = 还没捕获过
}

var (
	capMu   sync.Mutex
	lastCap Capture
)

type winmsg struct {
	hwnd, message, wParam, lParam uintptr
	time, ptX, ptY                uint32
}

func hotkeyLoop() {
	runtime.LockOSThread() // 注册热键的线程必须自己泵消息
	// id=1, 无修饰键(0x4000=MOD_NOREPEAT), VK_F8=0x77
	ok, _, _ := procRegisterHotKey.Call(0, 1, 0x4000, 0x77)
	if ok == 0 {
		fmt.Println("警告: F8 全局热键注册失败(可能被其他程序占用)")
		return
	}
	var m winmsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 {
			return
		}
		if m.message == 0x0312 { // WM_HOTKEY
			x, y := getCursor()
			capMu.Lock()
			lastCap = Capture{X: x, Y: y, Ts: time.Now().UnixMilli()}
			capMu.Unlock()
			fmt.Printf("捕获坐标: %d,%d\n", x, y)
		}
	}
}

// ---------- 数据模型 ----------

type Step struct {
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Offset  int    `json:"offset"`  // 随机偏移 ±N 像素
	Click   string `json:"click"`   // left / double / right
	PauseMs int    `json:"pauseMs"` // 本步点击后的暂停毫秒
}

type Task struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Loop           int    `json:"loop"`           // 0 = 无限循环
	LoopIntervalMs int    `json:"loopIntervalMs"` // 每轮之间的间隔
	Steps          []Step `json:"steps"`
}

type Status struct {
	Running   bool   `json:"running"`
	TaskID    string `json:"taskId"`
	TaskName  string `json:"taskName"`
	LoopNow   int    `json:"loopNow"`
	LoopTotal int    `json:"loopTotal"` // 0 = 无限
	StepNow   int    `json:"stepNow"`
	StepTotal int    `json:"stepTotal"`
	Message   string `json:"message"`
}

// ---------- 全局状态 ----------

var (
	dataFile = func() string {
		exe, err := os.Executable()
		if err != nil {
			return "tasks.json"
		}
		return filepath.Join(filepath.Dir(exe), "tasks.json")
	}()

	mu       sync.Mutex
	tasks    []Task
	status   Status
	stopCh   chan struct{}
	stopOnce sync.Once
)

func loadTasks() {
	b, err := os.ReadFile(dataFile)
	if err != nil {
		return
	}
	json.Unmarshal(b, &tasks)
}

func saveTasksLocked() {
	b, _ := json.MarshalIndent(tasks, "", "  ")
	os.WriteFile(dataFile, b, 0644)
}

func setStatus(f func(*Status)) {
	mu.Lock()
	f(&status)
	mu.Unlock()
}

// ---------- 执行引擎 ----------

const mouseMoveTolerance = 8

func sleepInterruptible(d time.Duration) bool {
	select {
	case <-stopCh:
		return false
	case <-time.After(d):
		return true
	}
}

func runTask(task Task) {
	defer setStatus(func(s *Status) { s.Running = false; s.Message = "已停止" })

	for loop := 1; task.Loop == 0 || loop <= task.Loop; loop++ {
		setStatus(func(s *Status) { s.LoopNow = loop; s.Message = "运行中" })

		for i, st := range task.Steps {
			// 急停检查：Web 停止按钮 或 ESC 键
			select {
			case <-stopCh:
				return
			default:
			}
			if escPressed() {
				setStatus(func(s *Status) { s.Message = "检测到 ESC，已急停" })
				return
			}

			x, y := st.X, st.Y
			if st.Offset > 0 {
				x += rand.IntN(2*st.Offset+1) - st.Offset
				y += rand.IntN(2*st.Offset+1) - st.Offset
			}
			setStatus(func(s *Status) { s.StepNow = i + 1; s.Message = "运行中" })
			setCursor(x, y)
			// 给系统 40~100ms 的随机反应时间，期间仍可通过停止按钮中断。
			delay := time.Duration(40+rand.IntN(61)) * time.Millisecond
			if !sleepInterruptible(delay) {
				return
			}

			// 如果等待期间鼠标被人工移开，只跳过本次点击，任务继续。
			actualX, actualY := getCursor()
			if actualX < x-mouseMoveTolerance || actualX > x+mouseMoveTolerance ||
				actualY < y-mouseMoveTolerance || actualY > y+mouseMoveTolerance {
				setStatus(func(s *Status) { s.Message = fmt.Sprintf("第 %d 步检测到鼠标移动，已跳过点击", i+1) })
			} else {
				doClick(st.Click)
			}

			if st.PauseMs > 0 && !sleepInterruptible(time.Duration(st.PauseMs) * time.Millisecond) {
				return
			}
		}

		// 还有下一轮才等待轮间隔
		if task.Loop == 0 || loop < task.Loop {
			if task.LoopIntervalMs > 0 {
				setStatus(func(s *Status) { s.Message = "等待下一轮" })
				if !sleepInterruptible(time.Duration(task.LoopIntervalMs) * time.Millisecond) {
					return
				}
			}
		}
	}
	setStatus(func(s *Status) { s.Message = "任务完成" })
}

func startTask(id string) error {
	mu.Lock()
	defer mu.Unlock()
	if status.Running {
		return fmt.Errorf("已有任务在运行，请先停止")
	}
	var found *Task
	for i := range tasks {
		if tasks[i].ID == id {
			found = &tasks[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("任务不存在")
	}
	if len(found.Steps) == 0 {
		return fmt.Errorf("任务没有任何步骤")
	}
	stopCh = make(chan struct{})
	stopOnce = sync.Once{}
	t := *found
	status.Running = true
	status.TaskID = t.ID
	status.TaskName = t.Name
	status.LoopTotal = t.Loop
	status.StepTotal = len(t.Steps)
	go runTask(t)
	return nil
}

func stopTask() {
	mu.Lock()
	defer mu.Unlock()
	if !status.Running || stopCh == nil {
		return
	}
	stopOnce.Do(func() { close(stopCh) })
}

// ---------- HTTP ----------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func main() {
	rand.NewPCG(uint64(time.Now().UnixNano()), uint64(os.Getpid()))
	go hotkeyLoop()
	go overlayLoop()
	loadTasks()

	// 静态页面
	sub, _ := fs.Sub(staticFS, "static")
	http.Handle("/", http.FileServer(http.FS(sub)))

	// 任务列表
	http.HandleFunc("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, tasks)
		case http.MethodPost: // 整体保存（增删改都由前端改好后提交）
			var list []Task
			if err := json.NewDecoder(r.Body).Decode(&list); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			tasks = list
			saveTasksLocked()
			writeJSON(w, map[string]bool{"ok": true})
		}
	})

	// 启动任务
	http.HandleFunc("/api/start", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID string `json:"id"` }
		json.NewDecoder(r.Body).Decode(&req)
		if err := startTask(req.ID); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	// 停止
	http.HandleFunc("/api/stop", func(w http.ResponseWriter, r *http.Request) {
		stopTask()
		writeJSON(w, map[string]bool{"ok": true})
	})

	// 运行状态
	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		s := status
		mu.Unlock()
		writeJSON(w, s)
	})

	// 当前鼠标位置（供前端"拾取坐标"用）
	http.HandleFunc("/api/curpos", func(w http.ResponseWriter, r *http.Request) {
		x, y := getCursor()
		writeJSON(w, map[string]int{"x": x, "y": y})
	})

	// F8 捕获的坐标
	http.HandleFunc("/api/lastcap", func(w http.ResponseWriter, r *http.Request) {
		capMu.Lock()
		c := lastCap
		capMu.Unlock()
		writeJSON(w, c)
	})

	// 导出任务 JSON
	http.HandleFunc("/api/export", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		b, _ := json.MarshalIndent(tasks, "", "  ")
		mu.Unlock()
		w.Header().Set("Content-Disposition", `attachment; filename="autoclicker_tasks.json"`)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(b)
	})

	// 导入任务 JSON（替换全部）
	http.HandleFunc("/api/import", func(w http.ResponseWriter, r *http.Request) {
		var list []Task
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := json.Unmarshal(body, &list); err != nil {
			writeJSON(w, map[string]string{"error": "JSON 解析失败: " + err.Error()})
			return
		}
		mu.Lock()
		tasks = list
		saveTasksLocked()
		mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	})

	fmt.Println("自动点击器已启动: http://127.0.0.1:8899")
	fmt.Println("提示: 任务运行中随时按 ESC 键急停")
	if err := http.ListenAndServe("127.0.0.1:8899", nil); err != nil {
		fmt.Println("启动失败:", err)
	}
}
