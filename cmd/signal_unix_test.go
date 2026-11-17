//go:build !windows

package cmd

import (
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSetupSignalsHUPSetsRotateFlag 验证 SIGHUP → rotateFlag.Store(true)
// （§4.6：SIGHUP 设置 atomic flag，下次 Write 时触发切分）。
// 测试进程自身接收信号：setupSignals 已注册 Notify，信号被截获不会杀进程。
func TestSetupSignalsHUPSetsRotateFlag(t *testing.T) {
	var rotateFlag atomic.Bool
	_, stop := setupSignals(&rotateFlag)
	defer stop()

	self, err := os.FindProcess(os.Getpid())
	require.NoError(t, err)
	require.NoError(t, self.Signal(syscall.SIGHUP))

	require.Eventually(t, rotateFlag.Load, 2*time.Second, 5*time.Millisecond,
		"SIGHUP must set the rotate flag")
}

// TestSetupSignalsTermCancelsContext 验证 SIGTERM → ctx 取消（§4.6：
// SIGTERM 优雅关停，cancel 通知主循环退出；§14.1-9 实现面）。
func TestSetupSignalsTermCancelsContext(t *testing.T) {
	var rotateFlag atomic.Bool
	ctx, stop := setupSignals(&rotateFlag)
	defer stop()

	self, err := os.FindProcess(os.Getpid())
	require.NoError(t, err)
	require.NoError(t, self.Signal(syscall.SIGTERM))

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("SIGTERM must cancel the context")
	}
	require.False(t, rotateFlag.Load(), "SIGTERM must not set the rotate flag")
}

// TestSetupSignalsStopIsIdempotent 验证 stop 可安全重复调用（defer 场景），
// 且 stop 后 context 已取消、无 goroutine 泄漏路径（signal.Stop 已注销）。
func TestSetupSignalsStopIsIdempotent(t *testing.T) {
	var rotateFlag atomic.Bool
	ctx, stop := setupSignals(&rotateFlag)

	stop()
	stop()

	select {
	case <-ctx.Done():
	default:
		t.Fatal("stop must cancel the context")
	}
}
