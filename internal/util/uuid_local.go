// Package util 中的 uuid_local.go 提供零外部依赖的 UUID v4/v5 生成。
//
// 设计取舍：
//   - 项目仅需内部追踪/会话分桶，不要求 RFC 4122 合规校验或解析。
//   - 不引入 google/uuid，避免增加依赖与编译产物体积。
//   - 实现风格与原 internal/app/codex_session_cache.go、
//     internal/protocol/builtin/request_prompt.go 中两份手写实现统一为一处，
//     消除位运算与格式化逻辑重复（DRY）。
package util

import (
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // UUIDv5 per RFC 4122 requires SHA-1
	"fmt"
)

// NameSpaceOID 是 RFC 4122 定义的 OID namespace UUID，可作为 NewUUIDv5 的 namespace 参数。
var NameSpaceOID = [16]byte{
	0x6b, 0xa7, 0xb8, 0x12, 0x9d, 0xad, 0x11, 0xd1,
	0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

// nilUUIDv4 是 rand.Read 失败时的兜底返回值（保持 v4 形态以便下游解析不崩）。
const nilUUIDv4 = "00000000-0000-4000-8000-000000000000"

// NewUUIDv4 生成随机 UUID v4 字符串。
// rand.Read 失败时返回 nilUUIDv4（极不可能发生；调用方按字面量比较即可识别）。
func NewUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nilUUIDv4
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return formatUUID(b)
}

// NewUUIDv5 基于 namespace + name 生成确定性 UUID v5（SHA-1）。
func NewUUIDv5(namespace [16]byte, name string) string {
	h := sha1.New() //nolint:gosec // UUIDv5 by spec
	h.Write(namespace[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50 // version 5
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
