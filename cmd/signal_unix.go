//go:build !windows

package cmd

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

// setupSignals 监听 SIGHUP(设置 rotateFlag)、SIGTERM/SIGINT(cancel context)
// （§4.6、§7.2）。SIGTERM/SIGINT 走 stdlib signal.NotifyContext（缓冲通道与
// 注销由其内部管理）；SIGHUP 分支独立监听：每次到达仅置位 atomic flag，
// 由下一次 Write 消费（§4.2 触发条件 3）。返回的 stop 注销 SIGHUP 监听并
// cancel ctx——SIGHUP goroutine 经 ctx.Done() 退出，无 goroutine 泄漏。
func setupSignals(rotateFlag *atomic.Bool) (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-hup:
				rotateFlag.Store(true)
			case <-ctx.Done():
				return
			}
		}
	}()

	return ctx, func() {
		signal.Stop(hup)
		cancel()
	}
}
