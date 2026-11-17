//go:build windows

package rotator

import "os"

// symlinkPlatformNote 是 Windows 平台降级提示（§4.5/§7.1）：创建 symlink
// 需要开发者模式或管理员权限，权限不足时静默跳过；Verbose 模式下由 New
// 在启动时输出一次。
const symlinkPlatformNote = MsgPrefix + "symlink on Windows requires Developer Mode or administrator privileges; failures are silently ignored"

// updateSymlink 是 Windows 降级实现（§4.5）：任何失败静默返回 nil，
// 不影响主写入流程。M3b 同样生效——link 槽位被真实文件/目录占用时
// 拒绝替换（直接跳过且不删除），防止误删用户文件。链接内容复用共享的
// symlinkDest 归一规则（同目录相对/跨目录绝对，P1-1/GAP-P2-04）：
// 相对 target 按 link 目录解析，跨目录时不归一会产生悬空链接。
func updateSymlink(target, link string) error {
	if st, err := os.Lstat(link); err == nil && st.Mode()&os.ModeSymlink == 0 {
		return nil // M3b: 真实文件/目录，拒绝替换（静默降级）
	}
	dest, err := symlinkDest(target, link)
	if err != nil {
		return nil // 路径归一失败同样静默降级（保持平台语义）
	}
	os.Remove(link)
	os.Symlink(dest, link) // 权限不足等失败一律静默
	return nil
}
