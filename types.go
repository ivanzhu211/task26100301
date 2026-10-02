// Package chronobase 实现 ChronoBase 封存时序块存储库。
//
// 每个封存块是一个本地文件，保存单一指标、单一来源序列在一个 UTC 小时内的
// 全部遥测点。数据经分块压缩存储，支持完整顺序读回与按时间区间的局部读取。
package chronobase

import "errors"

// Point 是一条遥测观测：有符号 64 位纳秒时间戳（UTC）与 float64 值。
type Point struct {
	Timestamp int64
	Value     float64
}

// Meta 描述封存块的元信息。一个块只含一个指标的一条来源序列。
type Meta struct {
	Metric string // 指标名，不能为空
	Source string // 来源序列标识，不能为空
}

// 错误定义。
var (
	ErrEmptyMetric    = errors.New("chronobase: metric 不能为空")
	ErrEmptySource    = errors.New("chronobase: source 不能为空")
	ErrNotSealed      = errors.New("chronobase: 块尚未封存")
	ErrAlreadySealed  = errors.New("chronobase: 块已封存，不能再写入")
	ErrTimestampOrder = errors.New("chronobase: 时间戳必须非递减")
	ErrHourMismatch   = errors.New("chronobase: 所有点必须属于同一 UTC 小时")
	ErrInvalidRange   = errors.New("chronobase: 区间必须满足 start < end")
	ErrInvalidFile    = errors.New("chronobase: 不是有效的封存块文件")
	ErrUnsupportedVer = errors.New("chronobase: 不支持的文件版本")
	ErrCorrupt        = errors.New("chronobase: 文件数据损坏")
)
