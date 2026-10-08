package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"
)

// RAWListener 实现标准 TCP/IP 打印端口协议（RAW，JetDirect 9100）。
// Windows 客户端“标准 TCP/IP 打印端口”直连此端口即可打印。
type RAWListener struct {
	Port     int
	Printer  string
	backend  Backend
	tracker  *JobTracker
	log      *Logger
	maxBytes int64
	idle     time.Duration
}

func (s *RAWListener) handle(conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	ip := hostPart(remote)

	if !s.backend.HasPrinter(s.Printer) {
		s.log.Warn("打印机「%s」当前不可用，拒绝来自 %s 的连接", s.Printer, ip)
		return
	}

	meta := JobMeta{
		Doc:    fmt.Sprintf("RAW-%s-%s", safeName(s.Printer), time.Now().Format("0102-150405")),
		Source: ip,
	}
	h, err := s.backend.OpenJob(s.Printer, meta)
	if err != nil {
		s.log.Error("打开打印作业失败（打印机 %s，来自 %s）: %v", s.Printer, ip, err)
		return
	}
	rec := s.tracker.Begin(s.Printer, meta.Doc, ip, "RAW")
	winID := h.JobID()
	if winID > 0 {
		s.tracker.SetWinJob(rec, winID)
	}

	var (
		total int64
		rerr  error
		buf   = make([]byte, 32*1024)
	)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.idle))
		n, err := conn.Read(buf)
		if n > 0 {
			if s.maxBytes > 0 && total+int64(n) > s.maxBytes {
				rerr = fmt.Errorf("作业超过最大限制 %s", byteSize(s.maxBytes))
				break
			}
			wn, werr := h.Write(buf[:n])
			total += int64(wn)
			if werr != nil {
				rerr = fmt.Errorf("写入打印队列失败: %w", werr)
				break
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if total > 0 {
					s.log.Warn("来自 %s 的作业空闲超时，按已接收的 %s 数据提交", ip, byteSize(total))
					break
				}
				rerr = fmt.Errorf("客户端空闲超时未发送数据")
				break
			}
			// 连接被重置等：有数据就提交，没数据按失败处理
			if total > 0 {
				s.log.Warn("来自 %s 的连接中断（%v），按已接收的 %s 数据提交", ip, err, byteSize(total))
				break
			}
			rerr = err
			break
		}
	}

	if rerr != nil {
		h.Abort()
		finishJob(s.tracker, s.backend, rec, total, winID, rerr)
		s.log.Error("RAW[%d] %s 来自 %s 的作业失败（%s）: %v", s.Port, s.Printer, ip, byteSize(total), rerr)
		return
	}
	if total == 0 {
		h.Abort()
		s.log.Warn("RAW[%d] 收到来自 %s 的空连接，已忽略", s.Port, ip)
		return
	}
	if err := h.Commit(); err != nil {
		finishJob(s.tracker, s.backend, rec, total, winID, err)
		s.log.Error("RAW[%d] %s 提交作业失败: %v", s.Port, s.Printer, err)
		return
	}
	finishJob(s.tracker, s.backend, rec, total, winID, nil)
	s.log.Info("RAW[%d] %s ← %s：%s 已接收", s.Port, s.Printer, ip, byteSize(total))
}

// Serve 在 ctx 取消或监听器关闭时返回。
func (s *RAWListener) Serve(ctx context.Context, ln net.Listener) {
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
			s.log.Warn("RAW[%d] accept 失败: %v", s.Port, err)
			// 短暂退避，避免忙循环
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
