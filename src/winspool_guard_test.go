package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestEnumPrintersLevel 防止回归：EnumPrinters 的 Level 参数必须非 0。
// 传 0 会拿到与 PRINTER_INFO_4 不匹配的缓冲区，导致解析野指针直接崩溃（v1.0.0 的线上事故）。
func TestEnumPrintersLevel(t *testing.T) {
	src, err := os.ReadFile("winspool_windows.go")
	if err != nil {
		t.Fatalf("读取 winspool_windows.go 失败: %v", err)
	}
	re := regexp.MustCompile(`procEnumPrinters\.Call\(([^)]*)`)
	ms := re.FindAllStringSubmatch(string(src), -1)
	if len(ms) == 0 {
		t.Fatal("未找到 EnumPrinters 调用，源码结构已变，请更新本测试")
	}
	for _, m := range ms {
		args := strings.Split(m[1], ",")
		if len(args) < 3 {
			t.Fatalf("EnumPrinters 调用参数不足: %q", m[1])
		}
		lvl := strings.TrimSpace(args[2])
		if lvl == "0" || lvl == "" {
			t.Fatalf("EnumPrinters 的 Level 参数不能为 0/空，必须是 4(PRINTER_INFO_4)：%q", m[1])
		}
	}
}

// TestWinspoolBufferReadsAreBounded 防止回归：winspool 返回缓冲区里的字符串读取必须带边界校验。
func TestWinspoolBufferReadsAreBounded(t *testing.T) {
	data, err := os.ReadFile("winspool_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	if strings.Contains(src, "func utf16ToString(") {
		t.Fatal("不要使用无边界校验的 utf16ToString，请用 utf16InBuffer")
	}
	if !strings.Contains(src, "func utf16InBuffer(") {
		t.Fatal("缺少带边界校验的 utf16InBuffer")
	}
	// EnumJobs/GetPrinter 的缓冲区不得用裸 unsafe.Add 做步进解析
	if strings.Contains(src, "unsafe.Add(") {
		t.Fatal("请使用 unsafe.Slice 的类型化切片遍历，不要用 unsafe.Add 手动算偏移")
	}
}

// TestWinspoolUsesSetJobW 防止回归：winspool.drv 只导出 SetJobA/SetJobW，没有裸 SetJob
func TestWinspoolUsesSetJobW(t *testing.T) {
	data, err := os.ReadFile("winspool_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	if !strings.Contains(src, `procSetJob             = dllWinspool.NewProc("SetJobW")`) &&
		!regexp.MustCompile(`procSetJob\s*=\s*\w+\.NewProc\("SetJobW"\)`).MatchString(src) {
		t.Fatal(`SetJob 必须解析为 "SetJobW"（裸 "SetJob" 在 winspool.drv 中不存在）`)
	}
}
