package util

import (
	"log"
	"sync"
	"time"
)

// LoginRateLimiter 登录速率限制器（防暴力破解）
// 设计原则：
// - 基于IP地址限制：防止单个IP暴力破解
// - 指数退避：失败次数越多，锁定时间越长
// - 自动清理：1小时后重置计数器
// 支持优雅关闭
type LoginRateLimiter struct {
	attempts map[string]*attemptRecord // IP -> 尝试记录
	mu       sync.RWMutex

	// 配置参数
	maxAttempts     int           // 最大尝试次数（默认5次）
	lockoutDuration time.Duration // 锁定时长（默认15分钟）
	resetInterval   time.Duration // 计数重置间隔（默认1小时）

	// 优雅关闭机制
	stopCh chan struct{} // 关闭信号
}

// attemptRecord 尝试记录
type attemptRecord struct {
	count       int       // 失败次数
	lastAttempt time.Time // 最后尝试时间
	lockUntil   time.Time // 锁定截止时间
}

// NewLoginRateLimiter 创建登录速率限制器
func NewLoginRateLimiter() *LoginRateLimiter {
	limiter := &LoginRateLimiter{
		attempts:        make(map[string]*attemptRecord),
		maxAttempts:     5,                   // 最大5次尝试
		lockoutDuration: 15 * time.Minute,    // 锁定15分钟
		resetInterval:   1 * time.Hour,       // 1小时后重置
		stopCh:          make(chan struct{}), // 初始化关闭信号
	}

	// 启动后台清理协程（每小时清理过期记录）
	// 支持优雅关闭
	go limiter.cleanupLoop()

	return limiter
}

// AllowAttempt 检查是否允许尝试登录
// 返回值：true=允许，false=拒绝（被锁定）
func (rl *LoginRateLimiter) AllowAttempt(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	record, exists := rl.attempts[ip]

	// 首次尝试
	if !exists {
		rl.attempts[ip] = &attemptRecord{
			count:       1,
			lastAttempt: now,
		}
		return true
	}

	// 检查是否被锁定
	if now.Before(record.lockUntil) {
		return false
	}

	// 重置计数（超过1小时）
	if now.Sub(record.lastAttempt) > rl.resetInterval {
		record.count = 0
	}

	// 增加尝试次数
	record.count++
	record.lastAttempt = now

	// 超过最大次数，锁定
	if record.count > rl.maxAttempts {
		record.lockUntil = now.Add(rl.lockoutDuration)
		return false
	}

	return true
}

// RecordSuccess 记录成功登录（重置计数）
func (rl *LoginRateLimiter) RecordSuccess(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// 成功登录后，清除该IP的尝试记录
	delete(rl.attempts, ip)
}

// GetLockoutTime 获取锁定剩余时间（秒）
// 返回值：0=未锁定，>0=锁定剩余秒数
func (rl *LoginRateLimiter) GetLockoutTime(ip string) int {
	rl.mu.RLock()
	defer rl.mu.RUnlock()

	record, exists := rl.attempts[ip]
	if !exists {
		return 0
	}

	now := time.Now()
	if now.Before(record.lockUntil) {
		return int(record.lockUntil.Sub(now).Seconds())
	}

	return 0
}

// GetAttemptCount 获取当前尝试次数
func (rl *LoginRateLimiter) GetAttemptCount(ip string) int {
	rl.mu.RLock()
	defer rl.mu.RUnlock()

	record, exists := rl.attempts[ip]
	if !exists {
		return 0
	}

	// 检查是否已过期
	if time.Since(record.lastAttempt) > rl.resetInterval {
		return 0
	}

	return record.count
}

// cleanupLoop 定期清理过期记录（后台协程）
// 支持优雅关闭
func (rl *LoginRateLimiter) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rl.cleanup()
		case <-rl.stopCh:
			// 收到关闭信号，执行最后一次清理后退出
			rl.cleanup()
			return
		}
	}
}

// cleanup 清理过期记录
func (rl *LoginRateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	toDelete := make([]string, 0)

	for ip, record := range rl.attempts {
		// 清理条件：
		// 1. 超过重置间隔且未被锁定
		// 2. 锁定已过期且超过重置间隔
		if now.Sub(record.lastAttempt) > rl.resetInterval && now.After(record.lockUntil) {
			toDelete = append(toDelete, ip)
		}
	}

	for _, ip := range toDelete {
		delete(rl.attempts, ip)
	}

	if len(toDelete) > 0 {
		log.Printf("🧹 登录速率限制器：清理 %d 条过期记录", len(toDelete))
	}
}

// 优雅关闭LoginRateLimiter
// Stop 停止cleanupLoop后台协程
func (rl *LoginRateLimiter) Stop() {
	close(rl.stopCh)
}
