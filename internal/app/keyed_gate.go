package app

import (
	"context"
	"sync"
)

/**
 * @brief 按业务身份键串行化操作，并允许等待者取消。
 * @param ctx 等待期间的取消与截止时间上下文。
 * @param gates 当前身份域专用的非 nil 锁表，值必须为容量一的 chan struct{}。
 * @param key 该身份域中的可比较业务键。
 * @return 成功时返回必须调用一次的释放函数；等待取消时为 nil。
 * @return 等待取消时返回 context.Canceled 或 context.DeadlineExceeded；成功为 nil。
 * @throw panic ctx 或 gates 为 nil、key 不可比较、锁表值不是 chan struct{} 或发送时通道已关闭。
 * @note 获取后应立即 defer 释放；不同身份域必须使用不同锁表，锁表条目保留以避免并发等待者分裂到不同锁。
 */
func acquireKeyedGate(ctx context.Context, gates *sync.Map, key any) (func(), error) {
	gateValue, ok := gates.Load(key)
	if !ok {
		gateValue, _ = gates.LoadOrStore(key, make(chan struct{}, 1))
	}
	gate := gateValue.(chan struct{})
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
