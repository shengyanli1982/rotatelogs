//go:build !windows

package rotator

import (
	"errors"
	"fmt"
	"os"
)

// symlinkPlatformNote 是平台降级提示。unix 完整支持 symlink，无提示（§7.1）。
const symlinkPlatformNote = ""

// updateSymlink 创建/更新指向 target 的符号链接（§4.5）：同目录使用相对路径、
// 跨目录使用绝对路径；经 link+".tmp" 临时链接 + Rename 原子替换，避免读端
// 观察到空窗。M3b：link 槽位已被非 symlink（真实文件/目录）占用时拒绝替换
// 并返回 error，防止误删用户文件。tmp 槽位同施 M3b 防护（2026-11-16 审计
// P1-B）：symlink+".tmp" 命中日志文件名等碰撞形态时，无条件 os.Remove(tmp)
// 会解链活跃文件，写入进入孤儿 inode、Close 后数据静默丢失——Lstat 判定
// 非 symlink 占用即拒绝删除，与正式槽位同一防御哲学。
func updateSymlink(target, link string) error {
	if err := checkSymlinkSlot(link); err != nil {
		return err
	}
	dest, err := symlinkDest(target, link)
	if err != nil {
		return err
	}
	tmp := link + symlinkTmpSuffix
	// M3b 同款 tmp 槽位防护（P1-B）：真实文件/目录占位 → 拒绝清理；
	// 既有 symlink（含悬空）→ 允许 Remove+重建（原子替换流程的一部分）。
	if err := checkSymlinkSlot(tmp); err != nil {
		return err
	}
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(MsgPrefix+"remove %s: %w", tmp, err)
	}
	if err := os.Symlink(dest, tmp); err != nil {
		return fmt.Errorf(MsgPrefix+"symlink %s -> %s: %w", tmp, dest, err)
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp) // 尽力回收临时链接
		return fmt.Errorf(MsgPrefix+"rename %s -> %s: %w", tmp, link, err)
	}
	return nil
}

// checkSymlinkSlot 执行 M3b 槽位校验：link 已存在且不是 symlink → 拒绝替换。
// 不存在（含悬空 symlink 之外的 ErrNotExist）→ 槽位空闲。updateSymlink 对
// 正式槽位与 tmp 临时槽位均调用本判定（P1-B 后者 2026-11-16 审计补齐）。
func checkSymlinkSlot(link string) error {
	st, err := os.Lstat(link)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf(MsgPrefix+"lstat %s: %w", link, err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf(MsgPrefix+"symlink path %s already exists and is not a symlink", link)
	}
	return nil
}
