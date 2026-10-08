package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"math/big"
	"strings"
	"time"
)

// randomHex 返回 n 字节的随机十六进制串
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 极端情况兜底（crypto/rand 失败基本不可能）
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405.000000000")))
	}
	return hex.EncodeToString(b)
}

// randomPassword 生成易读的随机管理员口令（去掉了容易混淆的 0/O/1/l/I）
func randomPassword(n int) string {
	const chars = "ABCDEFGHJKMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"
	if n < 6 {
		n = 6
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			return randomHex(n)
		}
		sb.WriteByte(chars[idx.Int64()])
	}
	return sb.String()
}

// hashPassword 生成 "盐$SHA256(盐+口令)" 格式的哈希
func hashPassword(pw string) string {
	salt := randomHex(16)
	sum := sha256.Sum256([]byte(salt + pw))
	return salt + "$" + hex.EncodeToString(sum[:])
}

// verifyPassword 恒定时间比较口令哈希
func verifyPassword(stored, pw string) bool {
	if stored == "" || pw == "" {
		return false
	}
	salt, want, ok := strings.Cut(stored, "$")
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(salt + pw))
	got := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
