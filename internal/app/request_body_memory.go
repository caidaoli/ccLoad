package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

/**
 * @brief 预算拒绝后停止 HTTP/1 上传，避免 Body.Close 排空慢正文而延迟 503。
 * @param c 将返回预算错误的 HTTP 请求。
 * @note HTTP/1 连接不再复用；HTTP/2 的正文关闭仅取消对应流，无需修改读取期限。
 */
func stopRejectedRequestUpload(c *gin.Context) {
	if c.Request.ProtoMajor == 1 {
		c.Header("Connection", "close")
		_ = http.NewResponseController(c.Writer).SetReadDeadline(time.Now())
	}
}

/**
 * @brief 当前实例的 HTTP 请求正文缓冲预算已耗尽，调用方应返回可重试的 503。
 */
var errRequestBodyMemoryBudget = errors.New("request body memory budget exhausted (setting: request_body_memory_budget_bytes)")

/**
 * @brief 跨 HTTP 请求共享的正文缓冲预算；不代表进程总堆上限。
 * @note limit 启动后不可变，used 可由不同请求并发更新。
 */
type requestBodyMemoryBudget struct {
	limit int64        ///< 缓冲容量与解码工作区的总预算，单位字节。
	used  atomic.Int64 ///< 仍由进行中请求持有的预留字节数。
}

/**
 * @brief 单个 HTTP handler 持有的正文预留，读取失败时也由 handler 统一释放。
 * @note 仅由该请求的 handler goroutine 使用，不可跨 goroutine 共享。
 */
type requestBodyMemoryReservation struct {
	budget *requestBodyMemoryBudget ///< Server 实例共享预算。
	bytes  int64                    ///< 当前请求仍持有的预留字节数。
}

/**
 * @brief 在分配正文内存前占用预算，不排队等待其他请求释放。
 * @param size 待分配字节数，必须非负。
 * @return 容量不足时返回 errRequestBodyMemoryBudget，否则返回 nil。
 */
func (r *requestBodyMemoryReservation) reserve(size int64) error {
	for {
		used := r.budget.used.Load()
		if size > r.budget.limit-used {
			return errRequestBodyMemoryBudget
		}
		if r.budget.used.CompareAndSwap(used, used+size) {
			r.bytes += size
			return nil
		}
	}
}

/**
 * @brief 归还已不再持有的临时缓冲或解码工作区预留。
 * @param size 本请求已预留且仅释放一次的字节数。
 */
func (r *requestBodyMemoryReservation) release(size int64) {
	r.bytes -= size
	r.budget.used.Add(-size)
}

/**
 * @brief handler 结束时归还剩余正文预留，包括解析失败及取消路径。
 * @note 必须在 handler 中 defer 调用，调用后不得再使用该预留。
 */
func (r *requestBodyMemoryReservation) close() {
	r.release(r.bytes)
}

/**
 * @brief 按实际缓冲容量读取有界正文，扩容期间同时预留新旧缓冲。
 * @param ctx 客户端请求上下文。
 * @param body 已完成 Content-Encoding 解码的正文。
 * @param maxBody 该入口允许的最大解码后字节数。
 * @param contentLength 明文已知长度；压缩或长度未知时使用 -1。
 * @return 完整正文；错误时返回 nil，已有预留仍归 handler 所有。
 * @return 超过单请求上限返回 errBodyTooLarge，预算不足返回 errRequestBodyMemoryBudget；其余返回上下文或读取错误。
 */
func (r *requestBodyMemoryReservation) read(ctx context.Context, body io.Reader, maxBody, contentLength int64) ([]byte, error) {
	if contentLength > maxBody {
		return nil, errBodyTooLarge
	}
	var data []byte
	if contentLength > 0 {
		capacity := min(max(contentLength, 512), maxBody)
		if err := r.reserve(capacity); err != nil {
			return nil, err
		}
		data = make([]byte, 0, capacity)
	}
	var probe [1]byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(data) == cap(data) {
			// 先确认还有数据，避免长度恰好等于容量时为 EOF 再申请一份缓冲。
			n, err := body.Read(probe[:])
			if n == 0 {
				if err == io.EOF {
					return data, nil
				}
				if err != nil {
					return nil, err
				}
				continue
			}
			if int64(len(data)) == maxBody {
				return nil, errBodyTooLarge
			}
			capacity := min(max(int64(cap(data))*2, 512, contentLength), maxBody)
			if reserveErr := r.reserve(capacity); reserveErr != nil {
				return nil, reserveErr
			}
			next := make([]byte, len(data)+n, capacity)
			copy(next, data)
			copy(next[len(data):], probe[:n])
			r.release(int64(cap(data)))
			data = next
			if err == io.EOF {
				return data, nil
			}
			if err != nil {
				return nil, err
			}
		}
		n, err := body.Read(data[len(data):cap(data)])
		data = data[:len(data)+n]
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
