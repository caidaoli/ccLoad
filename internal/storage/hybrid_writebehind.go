package storage

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const (
	primarySyncRetryDelay   = 10 * time.Second
	primarySyncTimeout      = 30 * time.Second
	primaryReconcileTimeout = 5 * time.Minute
	primarySyncMaxPending   = 10_000
	primaryFullSyncKey      = "state/full-reconcile"
)

// primarySyncTask describes a desired final state, not a database command.
// Replacing a task with the same key is therefore safe and bounds repeated writes.
type primarySyncTask struct {
	key         string
	op          string
	generation  uint64
	nextAttempt time.Time
	run         func(context.Context) error
	bestEffort  bool
	timeout     time.Duration
}

type primaryWriteBehind struct {
	mu             sync.Mutex
	tasks          map[string]*primarySyncTask
	nextGen        uint64
	wake           chan struct{}
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	retry          time.Duration
	timeout        time.Duration
	closed         bool
	initialize     func(context.Context) error
	maxPending     int
	reconcile      func(context.Context) error
	reconcileDirty func()
	failures       atomic.Uint64
	dropped        atomic.Uint64
	success        atomic.Int64
}

func newPrimaryWriteBehind(retry, timeout time.Duration) *primaryWriteBehind {
	return newPrimaryWriteBehindWithInitializer(retry, timeout, nil)
}

func newPrimaryWriteBehindWithInitializer(
	retry, timeout time.Duration,
	initialize func(context.Context) error,
) *primaryWriteBehind {
	ctx, cancel := context.WithCancel(context.Background())
	w := &primaryWriteBehind{
		tasks:      make(map[string]*primarySyncTask),
		wake:       make(chan struct{}, 1),
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
		retry:      retry,
		timeout:    timeout,
		initialize: initialize,
	}
	go w.loop()
	return w
}

func (w *primaryWriteBehind) configureReconcile(
	maxPending int,
	reconcile func(context.Context) error,
	reconcileDirty func(),
) {
	w.mu.Lock()
	w.maxPending = maxPending
	w.reconcile = reconcile
	w.reconcileDirty = reconcileDirty
	w.mu.Unlock()
}

func (w *primaryWriteBehind) enqueue(key, op string, run func(context.Context) error) {
	w.enqueueTask(key, op, false, run)
}

func (w *primaryWriteBehind) enqueueBestEffort(key, op string, run func(context.Context) error) {
	w.enqueueTask(key, op, true, run)
}

func (w *primaryWriteBehind) enqueueTask(key, op string, bestEffort bool, run func(context.Context) error) {
	if run == nil {
		return
	}
	w.mu.Lock()
	if w.closed {
		w.dropped.Add(1)
		w.mu.Unlock()
		return
	}
	if !bestEffort && w.reconcile != nil {
		if _, reconciling := w.tasks[primaryFullSyncKey]; reconciling {
			// Lock order is write-behind -> reconcile cursor. The worker never
			// holds the cursor lock while acquiring w.mu.
			if w.reconcileDirty != nil {
				w.reconcileDirty()
			}
			w.replaceReconcileTaskLocked()
			w.mu.Unlock()
			w.signal()
			return
		}
		if w.maxPending > 0 && w.tasks[key] == nil && w.regularPendingLocked() >= w.maxPending {
			for taskKey, task := range w.tasks {
				if !task.bestEffort {
					delete(w.tasks, taskKey)
				}
			}
			if w.reconcileDirty != nil {
				w.reconcileDirty()
			}
			w.replaceReconcileTaskLocked()
			w.mu.Unlock()
			w.signal()
			return
		}
	}
	if previous := w.tasks[key]; previous != nil && previous.bestEffort {
		w.dropped.Add(1)
	}
	w.nextGen++
	w.tasks[key] = &primarySyncTask{
		key:         key,
		op:          op,
		generation:  w.nextGen,
		nextAttempt: time.Now(),
		run:         run,
		bestEffort:  bestEffort,
	}
	w.mu.Unlock()
	w.signal()
}

func (w *primaryWriteBehind) regularPendingLocked() int {
	count := 0
	for _, task := range w.tasks {
		if !task.bestEffort {
			count++
		}
	}
	return count
}

func (w *primaryWriteBehind) replaceReconcileTaskLocked() {
	w.nextGen++
	w.tasks[primaryFullSyncKey] = &primarySyncTask{
		key:         primaryFullSyncKey,
		op:          "full state reconciliation",
		generation:  w.nextGen,
		nextAttempt: time.Now(),
		run:         w.reconcile,
		timeout:     primaryReconcileTimeout,
	}
}

func (w *primaryWriteBehind) continueReconcile() {
	w.mu.Lock()
	if w.closed || w.tasks[primaryFullSyncKey] == nil {
		w.mu.Unlock()
		return
	}
	w.replaceReconcileTaskLocked()
	w.mu.Unlock()
	w.signal()
}

func (w *primaryWriteBehind) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *primaryWriteBehind) loop() {
	defer close(w.done)
	if !w.initializePrimary() {
		return
	}
	for {
		task, wait := w.nextTask()
		if task == nil {
			if !w.wait(wait) {
				return
			}
			continue
		}

		timeout := task.timeout
		if timeout <= 0 {
			timeout = w.timeout
		}
		opCtx, cancel := context.WithTimeout(w.ctx, timeout)
		err := task.run(opCtx)
		cancel()
		if w.ctx.Err() != nil {
			return
		}
		w.finish(task, err)
	}
}

func (w *primaryWriteBehind) initializePrimary() bool {
	for {
		w.mu.Lock()
		initialize := w.initialize
		w.mu.Unlock()
		if initialize == nil {
			return true
		}
		err := initialize(w.ctx)
		if w.ctx.Err() != nil {
			return false
		}
		if err == nil {
			w.mu.Lock()
			w.initialize = nil
			w.mu.Unlock()
			w.success.Store(time.Now().UnixMilli())
			return true
		}
		failureCount := w.failures.Add(1)
		if failureCount%10 == 1 {
			log.Printf("[WARN] 主库后台初始化失败: %v；%s 后重试 (累计失败: %d)", err, w.retry, failureCount)
		}
		if !w.waitRetryDelay() {
			return false
		}
	}
}

func (w *primaryWriteBehind) waitRetryDelay() bool {
	timer := time.NewTimer(w.retry)
	defer timer.Stop()
	select {
	case <-w.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *primaryWriteBehind) nextTask() (*primarySyncTask, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.tasks) == 0 {
		return nil, 0
	}
	now := time.Now()
	var selected *primarySyncTask
	var earliest time.Time
	for _, task := range w.tasks {
		if !task.nextAttempt.After(now) {
			if selected == nil || task.generation < selected.generation {
				selected = task
			}
			continue
		}
		if earliest.IsZero() || task.nextAttempt.Before(earliest) {
			earliest = task.nextAttempt
		}
	}
	if selected != nil {
		return selected, 0
	}
	return nil, time.Until(earliest)
}

func (w *primaryWriteBehind) wait(delay time.Duration) bool {
	if delay <= 0 {
		select {
		case <-w.ctx.Done():
			return false
		case <-w.wake:
			return true
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-w.ctx.Done():
		return false
	case <-w.wake:
		return true
	case <-timer.C:
		return true
	}
}

func (w *primaryWriteBehind) finish(task *primarySyncTask, err error) {
	var failureCount uint64
	if err != nil {
		failureCount = w.failures.Add(1)
	}
	w.mu.Lock()
	current := w.tasks[task.key]
	if current == nil || current.generation != task.generation {
		w.mu.Unlock()
		if err != nil && failureCount%10 == 1 {
			log.Printf("[WARN] 主库后台同步失败 (%s): %v (累计失败: %d)", task.op, err, failureCount)
		}
		return
	}
	if err == nil {
		delete(w.tasks, task.key)
		w.success.Store(time.Now().UnixMilli())
		w.mu.Unlock()
		return
	}
	if current.bestEffort {
		delete(w.tasks, task.key)
		w.dropped.Add(1)
		w.mu.Unlock()
		if failureCount%10 == 1 {
			log.Printf("[WARN] 主库 best-effort 同步失败并丢弃 (%s): %v (累计失败: %d)", task.op, err, failureCount)
		}
		return
	}
	current.nextAttempt = time.Now().Add(w.retry)
	w.mu.Unlock()
	if failureCount%10 == 1 {
		log.Printf("[WARN] 主库后台同步失败 (%s): %v；%s 后重试 (累计失败: %d)", task.op, err, w.retry, failureCount)
	}
}

func (w *primaryWriteBehind) pending() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	pending := len(w.tasks)
	if w.initialize != nil {
		pending++
	}
	return pending
}

func (w *primaryWriteBehind) close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		<-w.done
		return
	}
	w.closed = true
	pending := len(w.tasks)
	if w.initialize != nil {
		pending++
	}
	w.dropped.Add(uint64(pending))
	w.mu.Unlock()
	w.cancel()
	<-w.done
}
