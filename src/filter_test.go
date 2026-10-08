package main

import (
	"net"
	"strings"
	"testing"
)

// TestPrinterFilterRealWorldCases 用 James 那台机器上的真实枚举结果做回归：
// 只应保留真实打印机，跳过 PDF/XPS/传真/虚拟绘图仪/笔记类。
func TestPrinterFilterRealWorldCases(t *testing.T) {
	f := newPrinterFilter(defaultConfig())
	cases := []struct {
		name string
		p    Printer
		keep bool
		why  string
	}{
		{"WPS 输出 PDF", Printer{Name: "导出为WPS PDF", Driver: "Kingsoft Virtual Printer Driver", Port: "Kingsoft Virtual Printer Port"}, false, "虚拟 PDF"},
		{"ZWCAD 虚拟绘图仪", Printer{Name: "ZWCAD Virtual Eps Plotter 1.0", Driver: "ZWCAD Virtual Eps Driver 1.0", Port: "FILE:"}, false, "虚拟绘图仪"},
		{"SHARP 网络一体机", Printer{Name: "SHARP MX-3128UC（财务）", Driver: "SHARP MX-3128UC", Port: "192.168.0.142"}, true, "真实网络打印机"},
		{"OneNote", Printer{Name: "OneNote for Windows 10", Driver: "Microsoft Software Printer Driver", Port: "Microsoft.Office.OneNote_..._onenoteim"}, false, "软件打印机"},
		{"RICOH 网络打印机", Printer{Name: "RICOH MP C6004 PCL 6", Driver: "RICOH MP C6004 PCL 6", Port: "IP_192.168.0.146"}, true, "真实网络打印机"},
		{"pdfFactory", Printer{Name: "pdfFactory Pro", Driver: "pdfFactory 7", Port: "FPP7:"}, false, "虚拟 PDF"},
		{"XPS Document Writer", Printer{Name: "Microsoft XPS Document Writer", Driver: "Microsoft XPS Document Writer v4", Port: "PORTPROMPT:"}, false, "虚拟 XPS"},
		{"Print to PDF", Printer{Name: "Microsoft Print to PDF", Driver: "Microsoft Print To PDF", Port: "PORTPROMPT:"}, false, "虚拟 PDF"},
		{"传真", Printer{Name: "Fax", Driver: "Microsoft Shared Fax Driver", Port: "SHRFAX:"}, false, "传真"},
		{"EPSON WSD 打印机", Printer{Name: "EPSON L3250 Series", Driver: "Microsoft IPP Class Driver", Port: "WSD-f1e49c33-1c80-41de-895a-c4456ed7478c"}, true, "真实网络打印机"},
		{"LQ-630K USB", Printer{Name: "EPSON LQ-630K", Driver: "EPSON LQ-630K ESC/P2", Port: "USB001"}, true, "针式打印机（USB）"},
		{"LQ-630K 并口", Printer{Name: "EPSON LQ-630K", Driver: "EPSON LQ-630K", Port: "LPT1:"}, true, "针式打印机（并口）"},
	}
	for _, c := range cases {
		if got := f.keep(c.p); got != c.keep {
			t.Errorf("%s: keep=%v，应为 %v（%s；驱动 %s；端口 %s）",
				c.name, got, c.keep, c.why, c.p.Driver, c.p.Port)
		}
	}
}

func TestPrinterFilterIncludeWhitelist(t *testing.T) {
	cfg := defaultConfig()
	cfg.IncludeKeywords = []string{"LQ-630K"}
	f := newPrinterFilter(cfg)
	if !f.keep(Printer{Name: "EPSON LQ-630K", Driver: "EPSON LQ-630K", Port: "USB001"}) {
		t.Fatal("白名单内的打印机应保留")
	}
	if f.keep(Printer{Name: "RICOH MP C6004 PCL 6", Driver: "RICOH MP C6004 PCL 6", Port: "IP_192.168.0.146"}) {
		t.Fatal("不在白名单内应跳过")
	}
}

func TestPrinterFilterCanBeDisabled(t *testing.T) {
	cfg := defaultConfig()
	cfg.KeepVirtualPrinters = true
	f := newPrinterFilter(cfg)
	if !f.keep(Printer{Name: "Microsoft Print to PDF", Driver: "Microsoft Print To PDF", Port: "PORTPROMPT:"}) {
		t.Fatal("keep_virtual_printers=true 时不应过滤")
	}
}

func TestPrinterFilterOfflineByName(t *testing.T) {
	f := newPrinterFilter(defaultConfig())
	if f.keepOfflineByName("Microsoft Print to PDF") {
		t.Fatal("离线的虚拟打印机也不该保留历史分配")
	}
	if !f.keepOfflineByName("EPSON LQ-630K") {
		t.Fatal("离线的真实打印机应保留历史分配")
	}
}

// TestResolveBindingsSkipsVirtualPrinters 端到端：虚拟打印机不占端口，也不出现在绑定里
func TestResolveBindingsSkipsVirtualPrinters(t *testing.T) {
	dir := t.TempDir()
	cfgPath, _ := writeTestConfig(t, dir, func(c *Config) {
		c.FilePrinters = []string{"EPSON LQ-630K", "Microsoft Print to PDF", "Fax"}
	})
	m := startTestManager(t, cfgPath)

	bs := m.Bindings()
	if len(bs) != 1 || bs[0].Printer != "EPSON LQ-630K" {
		t.Fatalf("应只暴露真实打印机，实际: %+v", bs)
	}
	if sk := m.SkippedPrinters(); len(sk) != 2 {
		t.Fatalf("应跳过 2 台虚拟打印机，实际: %+v", sk)
	}
}

// TestFirewallArgsUseEnableNotEnabled netsh 的参数是 enable=yes（写 enabled=yes 会报参数无效）
func TestFirewallArgsUseEnableNotEnabled(t *testing.T) {
	args := firewallAddArgs([]int{9100, 9101, 515, 8080})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "enabled=") {
		t.Fatalf("netsh 不支持 enabled=，必须用 enable=yes: %v", args)
	}
	for _, want := range []string{"enable=yes", "dir=in", "action=allow", "protocol=TCP",
		"localport=9100,9101,515,8080", "profile=any", "name=NetPrintServer"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("防火墙参数缺少 %q: %v", want, args)
		}
	}
	if got := firewallDeleteArgs(); strings.Join(got, " ") != "advfirewall firewall delete rule name=NetPrintServer" {
		t.Fatalf("删除规则参数异常: %v", got)
	}
}

// TestIPSortKeyPrefersRealLAN 169.254/虚拟网卡不能排在真实内网地址前面
func TestIPSortKeyPrefersRealLAN(t *testing.T) {
	lan := ipSortKey(net.ParseIP("192.168.0.19").To4(), "以太网")
	vbox := ipSortKey(net.ParseIP("192.168.56.1").To4(), "VirtualBox Host-Only Network")
	hyperv := ipSortKey(net.ParseIP("172.18.0.1").To4(), "vEthernet (Default Switch)")
	link := ipSortKey(net.ParseIP("169.254.83.107").To4(), "本地连接*")
	if lan >= vbox || lan >= hyperv || lan >= link {
		t.Fatalf("内网地址应最优先: lan=%d vbox=%d hyperv=%d link=%d", lan, vbox, hyperv, link)
	}
}
