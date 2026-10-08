//go:build windows

package main

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

var (
	dllWinspool         = syscall.NewLazyDLL("winspool.drv")
	procOpenPrinter     = dllWinspool.NewProc("OpenPrinterW")
	procClosePrinter    = dllWinspool.NewProc("ClosePrinter")
	procStartDocPrinter = dllWinspool.NewProc("StartDocPrinterW")
	procEndDocPrinter   = dllWinspool.NewProc("EndDocPrinter")
	procWritePrinter    = dllWinspool.NewProc("WritePrinter")
	procGetPrinter      = dllWinspool.NewProc("GetPrinterW")
	procEnumPrinters    = dllWinspool.NewProc("EnumPrintersW")
	procEnumJobs        = dllWinspool.NewProc("EnumJobsW")
	procSetJob          = dllWinspool.NewProc("SetJobW") // winspool 只导出 SetJobA/SetJobW
	procSetJobLegacy    = dllWinspool.NewProc("SetJob")  // 仅用于自检探测，不调用
)

const (
	prnAccessUse        = 0x00000008
	prnAccessAdminister = 0x00000004
	prnEnumLocal        = 0x00000002
	prnEnumConnections  = 0x00000004
	jobControlDelete    = 5 // winspool.h: JOB_CONTROL_DELETE (3 是 JOB_CONTROL_CANCEL，已废弃)
)

type printerDefaults struct {
	pDatatype     *uint16
	pDevMode      uintptr
	desiredAccess uint32
}

type docInfo1 struct {
	pDocName    *uint16
	pOutputFile *uint16
	pDatatype   *uint16
}

type printerInfo4 struct {
	pPrinterName *uint16
	pServerName  *uint16
	attributes   uint32
}

// printerInfo2 对应 winspool.h 的 PRINTER_INFO_2W（注意首个成员是 pServerName）
type printerInfo2 struct {
	pServerName         *uint16
	pPrinterName        *uint16
	pShareName          *uint16
	pPortName           *uint16
	pDriverName         *uint16
	pComment            *uint16
	pLocation           *uint16
	pDevMode            uintptr
	pSepFile            *uint16
	pPrintProcessor     *uint16
	pDatatype           *uint16
	pParameters         *uint16
	pSecurityDescriptor uintptr
	attributes          uint32
	priority            uint32
	defaultPriority     uint32
	startTime           uint32
	untilTime           uint32
	status              uint32
	cJobs               uint32
	averagePPM          uint32
}

// jobInfo1 对应 winspool.h 的 JOB_INFO_1W（尾部有 SYSTEMTIME Submitted）
type jobInfo1 struct {
	jobID        uint32
	pPrinterName *uint16
	pMachineName *uint16
	pUserName    *uint16
	pDocument    *uint16
	pDatatype    *uint16
	pStatus      *uint16
	status       uint32
	priority     uint32
	position     uint32
	totalPages   uint32
	pagesPrinted uint32
	submitted    [8]uint16
}

func openPrinter(name string, access uint32) (uintptr, error) {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	var h uintptr
	def := printerDefaults{desiredAccess: access}
	r, _, callErr := procOpenPrinter.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&h)),
		uintptr(unsafe.Pointer(&def)),
	)
	if r == 0 {
		return 0, fmt.Errorf("打开打印机「%s」失败: %v", name, callErr)
	}
	return h, nil
}

func closePrinter(h uintptr) {
	if h != 0 {
		procClosePrinter.Call(h)
	}
}

// WinspoolBackend 通过 Windows 打印后台处理程序转发作业
type WinspoolBackend struct {
	cfg     *Config
	tracker *JobTracker
	log     *Logger
}

func NewWinspoolBackend(cfg *Config, tracker *JobTracker, log *Logger) Backend {
	return &WinspoolBackend{cfg: cfg, tracker: tracker, log: log}
}

func (w *WinspoolBackend) Kind() string { return "windows" }

func (w *WinspoolBackend) ListPrinters() ([]Printer, error) {
	names, err := enumPrinterNames()
	if err != nil {
		return nil, err
	}
	out := make([]Printer, 0, len(names))
	for _, n := range names {
		out = append(out, printerDetails(n))
	}
	return out, nil
}

func (w *WinspoolBackend) HasPrinter(name string) bool {
	names, err := enumPrinterNames()
	if err != nil {
		return false
	}
	for _, n := range names {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

func (w *WinspoolBackend) StatusOf(name string) string {
	names, err := enumPrinterNames()
	if err != nil {
		return "未知"
	}
	for _, n := range names {
		if strings.EqualFold(n, name) {
			return printerDetails(n).State
		}
	}
	return "打印机不存在"
}

func (w *WinspoolBackend) OpenJob(printer string, meta JobMeta) (JobHandle, error) {
	// 尽量拿到删除权限（服务以 SYSTEM 运行时可拿到），失败则降级为只写
	h, err := openPrinter(printer, prnAccessUse|prnAccessAdminister)
	if err != nil {
		h, err = openPrinter(printer, prnAccessUse)
		if err != nil {
			return nil, err
		}
	}
	docName := meta.Doc
	if docName == "" {
		docName = "NetPrintServer"
	}
	pDoc, _ := syscall.UTF16PtrFromString(docName)
	pOut, _ := syscall.UTF16PtrFromString("")
	pType, _ := syscall.UTF16PtrFromString("RAW")
	di := docInfo1{pDocName: pDoc, pOutputFile: pOut, pDatatype: pType}
	r, _, callErr := procStartDocPrinter.Call(uintptr(h), 1, uintptr(unsafe.Pointer(&di)))
	if r == 0 {
		closePrinter(h)
		return nil, fmt.Errorf("StartDocPrinter 失败: %v", callErr)
	}
	return &winJob{h: h, id: uint32(r)}, nil
}

// Refresh 轮询本机各打印机队列，把作业状态回写到作业记录
func (w *WinspoolBackend) Refresh() {
	printers, err := w.ListPrinters()
	if err != nil {
		return
	}
	for _, p := range printers {
		jobs, err := enumJobs(p.Name)
		if err != nil {
			continue
		}
		missing := map[uint32]bool{}
		for _, rec := range w.tracker.Active() {
			if !strings.EqualFold(rec.Printer, p.Name) {
				continue
			}
			if _, ok := jobs[rec.WinJobID]; !ok {
				missing[rec.WinJobID] = true
			}
		}
		w.tracker.UpdateFromQueue(p.Name, jobs, missing)
	}
}

type winJob struct {
	h      uintptr
	id     uint32
	closed bool
}

func (j *winJob) JobID() uint32 { return j.id }

func (j *winJob) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var written uint32
	r, _, callErr := procWritePrinter.Call(
		j.h,
		uintptr(unsafe.Pointer(&p[0])),
		uintptr(len(p)),
		uintptr(unsafe.Pointer(&written)),
	)
	if r == 0 {
		return int(written), fmt.Errorf("WritePrinter 失败: %v", callErr)
	}
	return int(written), nil
}

func (j *winJob) Commit() error {
	if j.closed {
		return nil
	}
	j.closed = true
	r, _, callErr := procEndDocPrinter.Call(j.h)
	closePrinter(j.h)
	if r == 0 {
		return fmt.Errorf("EndDocPrinter 失败: %v", callErr)
	}
	return nil
}

func (j *winJob) Abort() {
	if j.closed {
		return
	}
	j.closed = true
	if j.id != 0 {
		// 作业已进入队列，尝试删除；无权限时忽略错误
		procSetJob.Call(j.h, uintptr(j.id), 0, 0, jobControlDelete)
	}
	closePrinter(j.h)
}

func enumPrinterNames() ([]string, error) {
	// 注意：Level 必须是 4（PRINTER_INFO_4），传 0 会拿到无法解析的缓冲区
	const level = 4
	flags := uintptr(prnEnumLocal | prnEnumConnections)
	var needed, count uint32
	bufSize := uint32(8192)
	buf := make([]byte, bufSize)
	r, _, callErr := procEnumPrinters.Call(flags, 0, level,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(bufSize),
		uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
	if r == 0 {
		if needed <= bufSize || needed > 64<<20 {
			return nil, fmt.Errorf("EnumPrinters 失败: %v (needed=%d)", callErr, needed)
		}
		bufSize = needed
		buf = make([]byte, bufSize)
		r, _, callErr = procEnumPrinters.Call(flags, 0, level,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(bufSize),
			uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
	}
	if r == 0 {
		return nil, fmt.Errorf("EnumPrinters 失败: %v", callErr)
	}

	sz := int(unsafe.Sizeof(printerInfo4{}))
	if max := len(buf) / sz; int(count) > max {
		count = uint32(max)
	}
	low := uintptr(unsafe.Pointer(&buf[0]))
	high := low + uintptr(len(buf))
	items := unsafe.Slice((*printerInfo4)(unsafe.Pointer(&buf[0])), int(count))
	out := make([]string, 0, len(items))
	for _, pi := range items {
		if name, ok := utf16InBuffer(pi.pPrinterName, low, high); ok && name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// utf16InBuffer 只在已知缓冲区内读取 UTF-16 字符串。
// 返回 (字符串, 指针是否落在缓冲区内)：指针越界时直接跳过，避免野指针导致进程崩溃。
func utf16InBuffer(p *uint16, low, high uintptr) (string, bool) {
	if p == nil {
		return "", false
	}
	addr := uintptr(unsafe.Pointer(p))
	if addr < low || addr+2 > high {
		return "", false
	}
	const maxChars = 2048
	s := unsafe.Slice(p, int((high-addr)/2))
	n := 0
	for n < len(s) && n < maxChars && s[n] != 0 {
		n++
	}
	if n == 0 {
		return "", true
	}
	return syscall.UTF16ToString(s[:n]), true
}

func printerDetails(name string) Printer {
	p := Printer{Name: name, State: "未知"}
	h, err := openPrinter(name, prnAccessUse)
	if err != nil {
		p.State = "打开失败"
		p.Detail = err.Error()
		return p
	}
	defer closePrinter(h)

	var needed uint32
	procGetPrinter.Call(uintptr(h), 2, 0, 0, uintptr(unsafe.Pointer(&needed)))
	if needed < 8 || needed > 64<<20 {
		p.Detail = fmt.Sprintf("GetPrinter 返回的长度异常: %d", needed)
		return p
	}
	buf := make([]byte, needed)
	r, _, callErr := procGetPrinter.Call(uintptr(h), 2,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(needed),
		uintptr(unsafe.Pointer(&needed)))
	if r == 0 {
		p.Detail = fmt.Sprintf("GetPrinter 失败: %v", callErr)
		return p
	}
	low := uintptr(unsafe.Pointer(&buf[0]))
	high := low + uintptr(len(buf))
	pi := (*printerInfo2)(unsafe.Pointer(&buf[0]))
	if s, ok := utf16InBuffer(pi.pPortName, low, high); ok {
		p.Port = s
	}
	if s, ok := utf16InBuffer(pi.pDriverName, low, high); ok {
		p.Driver = s
	}
	if s, ok := utf16InBuffer(pi.pShareName, low, high); ok && s != "" {
		p.Detail = "共享名: " + s
	}
	p.Jobs = int(pi.cJobs)
	p.State = printerStatusText(pi.status)
	return p
}

func printerStatusText(st uint32) string {
	if st == 0 {
		return "就绪"
	}
	var parts []string
	add := func(bit uint32, s string) {
		if st&bit != 0 {
			parts = append(parts, s)
		}
	}
	// 位定义见 winspool.h PRINTER_STATUS_*
	add(0x00000001, "已暂停")
	add(0x00000002, "错误")
	add(0x00000004, "正在删除")
	add(0x00000008, "卡纸")
	add(0x00000010, "缺纸")
	add(0x00000020, "手动进纸")
	add(0x00000040, "纸张问题")
	add(0x00000080, "脱机")
	add(0x00000100, "IO 活动")
	add(0x00000200, "忙")
	add(0x00000400, "正在打印")
	add(0x00000800, "出纸盒满")
	add(0x00001000, "不可用")
	add(0x00002000, "等待中")
	add(0x00004000, "处理中")
	add(0x00008000, "初始化中")
	add(0x00010000, "预热中")
	add(0x00020000, "墨粉低")
	add(0x00040000, "无墨粉")
	add(0x00080000, "页面故障")
	add(0x00100000, "需要用户处理")
	add(0x00200000, "内存不足")
	add(0x00400000, "盖板打开")
	add(0x00800000, "服务器状态未知")
	add(0x01000000, "节能中")
	if len(parts) == 0 {
		return fmt.Sprintf("状态 0x%08X", st)
	}
	return strings.Join(parts, "/")
}

func enumJobs(printer string) (map[uint32]uint32, error) {
	h, err := openPrinter(printer, prnAccessUse)
	if err != nil {
		return nil, err
	}
	defer closePrinter(h)

	const maxJobs = 256
	var needed, count uint32
	bufSize := uint32(maxJobs)*uint32(unsafe.Sizeof(jobInfo1{})) + 4096
	buf := make([]byte, bufSize)
	r, _, callErr := procEnumJobs.Call(uintptr(h), 0, maxJobs, 1,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(bufSize),
		uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
	if r == 0 && needed > bufSize {
		buf = make([]byte, needed)
		bufSize = needed
		r, _, callErr = procEnumJobs.Call(uintptr(h), 0, maxJobs, 1,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(bufSize),
			uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
	}
	if r == 0 {
		return nil, fmt.Errorf("EnumJobs(%s) 失败: %v", printer, callErr)
	}
	sz := int(unsafe.Sizeof(jobInfo1{}))
	if max := len(buf) / sz; int(count) > max {
		count = uint32(max)
	}
	items := unsafe.Slice((*jobInfo1)(unsafe.Pointer(&buf[0])), int(count))
	out := make(map[uint32]uint32, len(items))
	for _, j := range items {
		out[j.jobID] = j.status
	}
	return out, nil
}

// diagPrinters 自检：把 winspool 各调用的原始返回值打印出来，方便远程排障。
// 所有解析都走边界校验，任何异常只会打印提示，不会崩溃。
func diagPrinters() error {
	fmt.Println("[自检] winspool.drv 函数解析：")
	for _, p := range []*syscall.LazyProc{
		procOpenPrinter, procClosePrinter, procStartDocPrinter, procEndDocPrinter,
		procWritePrinter, procGetPrinter, procEnumPrinters, procEnumJobs, procSetJob,
	} {
		if err := p.Find(); err != nil {
			fmt.Printf("  - %-20s 未找到: %v\n", p.Name, err)
		} else {
			fmt.Printf("  - %-20s OK\n", p.Name)
		}
	}
	if err := procSetJobLegacy.Find(); err == nil {
		fmt.Println("  - SetJob（旧名）     存在（本程序用 SetJobW）")
	} else {
		fmt.Println("  - SetJob（旧名）     不存在（正常：winspool 只导出 SetJobA/SetJobW）")
	}

	fmt.Println("[自检] EnumPrinters(Flags=LOCAL|CONNECTIONS, Level=4)：")
	const level = 4
	flags := uintptr(prnEnumLocal | prnEnumConnections)
	var needed, count uint32
	bufSize := uint32(8192)
	buf := make([]byte, bufSize)
	r, _, callErr := procEnumPrinters.Call(flags, 0, level,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(bufSize),
		uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
	fmt.Printf("  第一次调用: ret=%d err=%v needed=%d count=%d\n", r, callErr, needed, count)
	if r == 0 && needed > bufSize && needed < 64<<20 {
		bufSize = needed
		buf = make([]byte, bufSize)
		r, _, callErr = procEnumPrinters.Call(flags, 0, level,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(bufSize),
			uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
		fmt.Printf("  扩容后调用: ret=%d err=%v needed=%d count=%d\n", r, callErr, needed, count)
	}
	if r == 0 {
		return fmt.Errorf("EnumPrinters 调用失败: %v", callErr)
	}

	names, err := enumPrinterNames()
	if err != nil {
		return err
	}
	fmt.Printf("[自检] 解析出 %d 台打印机：\n", len(names))
	for i, n := range names {
		d := printerDetails(n)
		fmt.Printf("  [%d] 「%s」状态=%s 端口=%s 驱动=%s 队列作业=%d %s\n",
			i+1, n, d.State, d.Port, d.Driver, d.Jobs, d.Detail)
		if h, err := openPrinter(n, prnAccessUse); err != nil {
			fmt.Printf("      打开句柄失败: %v\n", err)
		} else {
			closePrinter(h)
			fmt.Printf("      打开句柄: OK\n")
		}
		if jobs, err := enumJobs(n); err == nil && len(jobs) > 0 {
			fmt.Printf("      队列作业: %v\n", jobs)
		}
	}
	return nil
}
