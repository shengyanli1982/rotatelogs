package rotator

import (
	"fmt"
	"path/filepath"
)

// symlinkTmpSuffix 是 updateSymlink 原子替换的临时槽位后缀（§4.5）：
// 经 link+".tmp" 临时链接 + Rename 原子落位，避免读端观察到空窗。跨文件
// 同步契约（2026-11-16 审计 P1-B）：checkSymlinkDistinct（validate.go 的
// M3a 预检）必须以同一后缀推导同一槽位名——预检放行而运行期碰撞时，
// updateSymlink 的 os.Remove(tmp) 会解链命中的活跃文件、数据静默丢失，
// 两处必须共用本值。定义于无 build tag 的共享文件：validate.go（全平台）
// 与 symlink_unix.go（Unix）共同引用；Windows 降级实现（symlink_windows.go）
// 无 tmp 槽机制，不引用（碰撞形态在该平台不构成实际威胁，预检保守拒绝
// 仅为跨平台校验一致性）。
const symlinkTmpSuffix = ".tmp"

// symlinkDest 按 §4.5 计算链接内容：target 与 link 同目录 → 相对路径；
// 跨目录 → 绝对路径。无 build tag，unix 与 windows 的 updateSymlink 共用
// 同一归一规则（P1-1/GAP-P2-04：Windows 版此前直接使用 target，相对
// pattern + 跨目录 symlink 时链接按 link 目录解析 → 悬空且无告警）。
func symlinkDest(target, link string) (string, error) {
	if filepath.Dir(target) == filepath.Dir(link) {
		rel, err := filepath.Rel(filepath.Dir(link), target)
		if err != nil {
			return "", fmt.Errorf(MsgPrefix+"rel path %s -> %s: %w", link, target, err)
		}
		return rel, nil
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf(MsgPrefix+"abs path %s: %w", target, err)
	}
	return abs, nil
}
