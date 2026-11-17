package cmd

// bench_test.go —— pump 主循环端到端基准（性能工程阶段 1，2026-11-13）。
//
// 口径：聚合吞吐（b.SetBytes=每 op 泵送字节数 → 报告 MB/s）。链路为
// bytes.Reader（模拟已就绪的 stdin，无 pipe syscall 阻塞）→ bufio.Reader
// 4KB 块读 → Rotator.Write（生产守卫形态）→ bufio 64KB → 文件。
//
// 测量纪律：
//   - 逐轮在 StopTimer 内全量重建状态（Close + 清目录 + New + 新 Reader），
//     保证每 op 的归档/uniqueName 探测状态恒定；拆卸不计时（规则 3/8）。
//   - NoRotate-ProdBaseline 用生产默认形态：RotationTime=24h + 真实时钟
//     （Now=nil）+ MaxSize=100MB → 每 4KB Write 触发一次 time.Now 守卫。
//   - Rotate256KB 为放大形态（每 op 触发 4 次 rotate）：CLI --max-size
//     最小 1MB，本变体用于让 rotate 成本在端到端吞吐中可见，生产口径
//     换算时按数据量/MaxSize 比例折算。
//   - ns/op 仅作参考（darwin+APFS 文件系统密集路径跨进程漂移 ~38%）；
//     allocs/op 与 B/op 为确定性指标。darwin 平台标注：含 open/close
//     syscall，禁止外推 Linux。

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shengyanli1982/rotatelogs/internal/rotator"
)

// benchPumpSize 是每 op 泵送的数据量（1MB ≈ 8700 行真实感日志）。
const benchPumpSize = 1 << 20

// benchPumpData 生成确定性伪随机真实感日志文本（与 internal/rotator 基准
// 同构；跨包无法复用测试 helper，逻辑就地复制并固定同一 PCG 种子）。
func benchPumpData(size int) []byte {
	rng := rand.New(rand.NewPCG(42, 7))
	levels := [4]string{"INFO", "WARN", "ERROR", "DEBUG"}
	urls := [4]string{"/api/v1/items", "/api/v1/orders", "/healthz", "/api/v2/search"}
	var sb strings.Builder
	sb.Grow(size + 256)
	for i := 0; sb.Len() < size; i++ {
		fmt.Fprintf(&sb, "2026-11-13T%02d:%02d:%02d.%03dZ %s [svc.handler] request completed id=%06d dur_ms=%d path=%s\n",
			rng.IntN(24), rng.IntN(60), rng.IntN(60), rng.IntN(1000),
			levels[rng.IntN(4)], i, rng.IntN(500), urls[rng.IntN(4)])
	}
	return []byte(sb.String()[:size])
}

// BenchmarkPump 度量 stdin→Rotator 端到端泵送吞吐。
func BenchmarkPump(b *testing.B) {
	data := benchPumpData(benchPumpSize)
	cases := []struct {
		name       string
		pattern    string
		maxSize    int64
		rotTime    time.Duration
		now        func() time.Time
		symlinkOff bool
	}{
		{"NoRotate-ProdBaseline", "app-%Y-%m-%d.log", 100 << 20, 24 * time.Hour, nil, true},
		{"Rotate256KB", "app.log", 256 << 10, 0, nil, true},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			dir := b.TempDir()
			cfg := rotator.Config{
				Pattern:      filepath.Join(dir, tc.pattern),
				MaxSize:      tc.maxSize,
				RotationTime: tc.rotTime,
				Now:          tc.now,
			}
			var r *rotator.Rotator
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				if r != nil {
					if err := r.Close(); err != nil {
						b.Fatal(err)
					}
				}
				if err := os.RemoveAll(dir); err != nil {
					b.Fatal(err)
				}
				if err := os.Mkdir(dir, 0o755); err != nil {
					b.Fatal(err)
				}
				var err error
				if r, err = rotator.New(cfg); err != nil {
					b.Fatal(err)
				}
				reader := bytes.NewReader(data)
				b.StartTimer()
				if err := pump(context.Background(), reader, r); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if r != nil {
				if err := r.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
