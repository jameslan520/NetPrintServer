package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// LPDListener 实现 RFC 1179 行式打印机守护进程（LPR/LPD，端口 515）。
// 客户端可使用“LPR 端口监视器”连接，队列名见状态页。
type LPDListener struct {
	Port       int
	Resolve    func(queue string) (printer string, ok bool)
	ListQueues func() []RawBinding
	backend    Backend
	tracker    *JobTracker
	log        *Logger
	maxBytes   int64
	idle       time.Duration
	serverName string
	dataQuiet  time.Duration // 数据流静默多久视为结束（兼容不发 \2 的客户端）
}

const lpdBufLimit = 64 * 1024

func (s *LPDListener) handle(conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	ip := hostPart(remote)

	br := bufio.NewReaderSize(conn, lpdBufLimit)
	_ = conn.SetReadDeadline(time.Now().Add(s.idle))
	line, err := readLPDLine(br, 512)
	if err != nil {
		s.log.Warn("LPD[%d] %s 读取命令失败: %v", s.Port, ip, err)
		return
	}
	if len(line) == 0 {
		return
	}
	cmd := line[0]
	arg := strings.TrimSpace(strings.TrimRight(string(line[1:]), "\r"))

	switch cmd {
	case 1, 2:
		s.recvJob(conn, br, ip, arg, cmd == 2)
	case 3: // 删除作业（本服务不跟踪外部作业，按未找到回复）
		s.log.Info("LPD 收到删除请求 %q（来自 %s），未匹配到作业", arg, ip)
		_, _ = conn.Write([]byte("1\n"))
	case 4:
		s.queueStatus(conn, arg)
	case 5:
		s.serverStatus(conn)
	default:
		s.log.Warn("LPD[%d] %s 未知命令 0x%02x", s.Port, ip, cmd)
	}
}

func (s *LPDListener) recvJob(conn net.Conn, br *bufio.Reader, ip, queue string, printNow bool) {
	printer, ok := s.Resolve(queue)
	if !ok {
		s.log.Warn("LPD 队列不存在: %q（来自 %s）", queue, ip)
		_, _ = conn.Write([]byte("2\n")) // 队列不存在
		return
	}
	if !s.backend.HasPrinter(printer) {
		s.log.Warn("LPD 队列 %q 对应的打印机「%s」不可用（来自 %s）", queue, printer, ip)
		_, _ = conn.Write([]byte("1\n")) // 暂不接受
		return
	}
	if _, err := conn.Write([]byte("0\n")); err != nil {
		return
	}

	// 读取控制文件（到 \2 为止）
	_ = conn.SetReadDeadline(time.Now().Add(s.idle))
	ctrl, _, err := readUntilStop(br, 0x02, 1<<20)
	if err != nil {
		s.log.Warn("LPD %s 控制文件读取失败（来自 %s）: %v", printer, ip, err)
		return
	}
	host, user, job := parseLPDControl(ctrl)
	title := job
	if title == "" {
		title = fmt.Sprintf("LPD-%s", time.Now().Format("0102-150405"))
	}
	if _, err := conn.Write([]byte("0\n")); err != nil {
		return
	}

	// 数据文件（可能有多个，直到连接关闭）
	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.dataQuiet))
		data, _, err := readUntilStop(br, 0x02, s.maxBytes)
		n := int64(len(data))
		if err == errStopLimit {
			s.log.Error("LPD %s 作业超过最大限制 %s（来自 %s）", printer, byteSize(s.maxBytes), ip)
			return
		}
		if n == 0 {
			if err == io.EOF || err == nil {
				return // 连接正常结束，没有更多数据文件
			}
			// 静默超时且没有数据
			s.log.Warn("LPD %s 等待数据超时（来自 %s）: %v", printer, ip, err)
			return
		}

		src := ip
		if user != "" && host != "" {
			src = user + "@" + host
		} else if user != "" {
			src = user
		}
		meta := JobMeta{Doc: title, User: user, Source: src, Queue: queue}
		h, oerr := s.backend.OpenJob(printer, meta)
		if oerr != nil {
			s.log.Error("LPD %s 打开作业失败: %v", printer, oerr)
			_, _ = conn.Write([]byte("1\n"))
			return
		}
		rec := s.tracker.Begin(printer, title, src, "LPD")
		winID := h.JobID()
		if winID > 0 {
			s.tracker.SetWinJob(rec, winID)
		}
		if _, werr := h.Write(data); werr != nil {
			h.Abort()
			finishJob(s.tracker, s.backend, rec, n, winID, werr)
			s.log.Error("LPD %s 写入失败: %v", printer, werr)
			return
		}
		if cerr := h.Commit(); cerr != nil {
			finishJob(s.tracker, s.backend, rec, n, winID, cerr)
			s.log.Error("LPD %s 提交失败: %v", printer, cerr)
			_, _ = conn.Write([]byte("1\n"))
			return
		}
		finishJob(s.tracker, s.backend, rec, n, winID, nil)
		s.log.Info("LPD 队列 %s → %s ← %s：%s 已接收%s", queue, printer, src, byteSize(n),
			map[bool]string{true: "（立即打印）", false: ""}[printNow])
		if _, err := conn.Write([]byte("0\n")); err != nil {
			return
		}
		if err == io.EOF || err == nil {
			// 已经读到数据文件结尾且连接关闭/无更多
			if br.Buffered() == 0 {
				return
			}
		}
		// 尝试读取下一个数据文件；无数据（EOF/超时）则结束
		_, perr := br.Peek(1)
		if perr != nil {
			return
		}
	}
}

func (s *LPDListener) queueStatus(conn net.Conn, queue string) {
	printer, ok := s.Resolve(queue)
	var b strings.Builder
	fmt.Fprintf(&b, "Printer: %s\r\n", queue)
	if !ok {
		fmt.Fprintf(&b, "unknown printer %s\r\n", queue)
	} else {
		fmt.Fprintf(&b, "Printer: %s is %s\r\n", printer, s.backend.StatusOf(printer))
	}
	_, _ = conn.Write([]byte(b.String()))
}

func (s *LPDListener) serverStatus(conn net.Conn) {
	var b strings.Builder
	fmt.Fprintf(&b, "Server: %s\r\n", s.serverName)
	fmt.Fprintf(&b, "Protocol: RFC1179 LPD\r\n")
	fmt.Fprintf(&b, "Queues: %d\r\n", len(s.ListQueues()))
	_, _ = conn.Write([]byte(b.String()))
}

func (s *LPDListener) Serve(ctx context.Context, ln net.Listener) {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("LPD[%d] accept 失败: %v", s.Port, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		go s.handle(conn)
	}
}

// ---- 辅助 ----

var errStopLimit = fmt.Errorf("超过大小限制")

// readLPDLine 读一行（以 \n 结束），最多 limit 字节。
func readLPDLine(br *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for len(out) < limit {
		b, err := br.ReadByte()
		if err != nil {
			return out, err
		}
		if b == '\n' {
			return out, nil
		}
		out = append(out, b)
	}
	return out, fmt.Errorf("命令行过长")
}

// readUntilStop 读到 stop 字节、连接关闭或超过 limit。
// 返回 (数据, 是否读到stop, 错误)。
func readUntilStop(br *bufio.Reader, stop byte, limit int64) ([]byte, byte, error) {
	var out []byte
	for limit <= 0 || int64(len(out)) < limit {
		chunk, err := br.ReadSlice(stop)
		out = append(out, chunk...)
		if err == nil {
			if len(out) > 0 {
				out = out[:len(out)-1] // 去掉终止符本身
			}
			return out, stop, nil // 找到终止符
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			return out, 0, io.EOF
		}
		// 网络读错误（含超时）：已有数据则视为该数据文件结束
		if len(out) > 0 {
			return out, 0, err
		}
		return out, 0, err
	}
	return out, 0, errStopLimit
}

// parseLPDControl 解析 LPD 控制文件。
func parseLPDControl(b []byte) (host, user, job string) {
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimRight(ln, "\r")
		if ln == "" || ln[0] == 0x02 {
			continue
		}
		key := ln[0]
		val := strings.TrimSpace(ln[1:])
		switch key {
		case 'H':
			host = val
		case 'P':
			user = val
		case 'J':
			if val != "" {
				job = val
			}
		case 'f', 'c', 'm', 'l':
			if job == "" && val != "" {
				job = val
			}
		}
	}
	return
}
