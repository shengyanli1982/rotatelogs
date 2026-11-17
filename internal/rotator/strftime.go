package rotator

import "strings"

// strftimeReplacements 定义 strftime → Go time.Format 的映射（DESIGN §3.3/§6.2，禁止扩充）。
var strftimeReplacements = map[string]string{
	"%Y": "2006",
	"%m": "01",
	"%d": "02",
	"%H": "15",
	"%M": "04",
	"%S": "05",
}

// strftimeReplacer 单次扫描替换：字面量（如 "2006"）不会被级联二次替换，
// 且结果与 map 迭代顺序无关（六个 token 互不为前缀）。
var strftimeReplacer = newStrftimeReplacer()

func newStrftimeReplacer() *strings.Replacer {
	pairs := make([]string, 0, 2*len(strftimeReplacements))
	for token, layout := range strftimeReplacements {
		pairs = append(pairs, token, layout)
	}
	return strings.NewReplacer(pairs...)
}

// parsePattern 将 strftime 格式的 pattern 转换为 Go time.Format 布局字符串。
// 未识别的占位符（如 %Q）保持原样。
//
// 注：本函数是 DESIGN §6.2 模块接口的参考实现，不在生产文件名生成路径上——
// 生产路径走 formatName（rotator.go）的逐 token 扫描替换，因为整体
// time.Format(layout) 会把路径中的字面数字误解释为时间 token（如 /data2/ →
// /data13/，formatName 处有禁改注释与回归测试钉住）。两者共用
// strftimeReplacements 的 §3.3 六 token 映射，逐 token 语义一致。
func parsePattern(pattern string) string {
	return strftimeReplacer.Replace(pattern)
}

// HasTimePlaceholder 报告 pattern 是否含 §3.3 定义的任一时间占位符。
// 供 New 做 M2 校验：RotationTime > 0 时 pattern 必须含时间占位符。
// 2026-11-16 导出：cmd 层的 --rotation-time 隐式默认意图降级（方案 B）
// 需要同一判定，token 知识保持单一来源（禁止 cmd 层复制 token 集合）。
func HasTimePlaceholder(pattern string) bool {
	for token := range strftimeReplacements {
		if strings.Contains(pattern, token) {
			return true
		}
	}
	return false
}
