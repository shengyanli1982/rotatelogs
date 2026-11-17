// rotatelogs 从 stdin 读取日志数据，写入按 strftime pattern 命名、
// 依时间与大小切分的日志文件（DESIGN §1.1）。
package main

import "github.com/shengyanli1982/rotatelogs/cmd"

func main() {
	cmd.Execute()
}
