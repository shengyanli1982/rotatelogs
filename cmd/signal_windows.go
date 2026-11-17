//go:build windows

package cmd

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"
)

// setupSignals 仅监听 os.Interrupt (Ctrl+C)，返回 cancel context（§7.2）。
// Windows 无 SIGHUP/SIGTERM 语义（§7.1 平台差异矩阵）：rotateFlag 永不置位，
// 参数仅为与 unix 版保持签名一致。
func setupSignals(rotateFlag *atomic.Bool) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}
