// Package chronobase 提供 ChronoBase 的封存时序块存储能力。
//
// 边缘设备将停止写入的小时级遥测块通过本库压缩归档为本地文件；
// 运维侧可完整顺序读回，或对短区间做局部读取与解压。
// 每块只含一个指标的一条来源序列，所有点属于同一 UTC 小时。
package chronobase

import "time"

// Point 是单个观测点：有符号 64 位纳秒时间戳 + float64 值。
// 相同时间戳可出现任意多次，每次都代表独立观测。
type Point struct {
	Timestamp int64
	Value     float64
}

// Meta 描述一个封存块的元数据。
type Meta struct {
	Metric string // 指标名
	Source string // 来源序列标识
	Hour   int64  // 块所属 UTC 小时的起始纳秒时间戳
}

// HourStart 返回任意时间戳所属 UTC 小时的起始纳秒时间戳。
func HourStart(ts int64) int64 {
	const hourNs = int64(time.Hour)
	t := ts % hourNs
	if t < 0 {
		t += hourNs
	}
	return ts - t
}
