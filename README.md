# ChronoBase 封存时序块存储库

ChronoBase 将边缘设备上停止写入的小时级遥测块封存为本地压缩归档文件，
支持完整顺序读回与按时间区间的局部读取（只读取并解压覆盖查询区间的压缩子块）。

## 特性

- 每个封存块保存**一个指标的一条来源序列**，所有点属于同一 UTC 小时。
- 每点为**有符号 64 位纳秒时间戳 + float64 值**；浮点值按原始位型存储，
  正负零、正负无穷、非规格化数、携带不同载荷的 NaN 均逐位无损往返。
- 相同时间戳可出现任意多次，每次均为独立观测，不去重，读回顺序与写入一致。
- 时间区间左闭右开 `[start, end)`，边界处的全部重复观测都会返回。
- 完整读取与区间读取均为**逐点迭代**，区间读取只局部加载相关压缩子块。
- 块封存后不再修改；关闭后重新 `Open` 即可独立读取，无需外部服务。

## 文件格式

```
[文件头 48B][指标名][来源标识][压缩子块 0]...[压缩子块 N-1][子块索引][文件尾 16B]
```

- 每个压缩子块最多 1024 点：首点为有符号 varint 绝对时间戳，其后为非负
  varint 时间戳增量；每个时间戳后紧跟 8 字节小端 float64 原始位型，
  整体经 DEFLATE 压缩。
- 索引记录每个子块的时间范围、文件偏移、压缩长度与点数，区间读取据此
  二分定位，仅加载覆盖区间的子块。
- 文件尾记录索引位置，打开时只读取头部与索引，不触碰数据子块。

## 使用

```go
package main

import (
	"fmt"
	"chronobase"
)

func main() {
	// 创建并封存（按时间戳非递减顺序逐点写入）。
	w, err := chronobase.Create("2026-09-30T14.sensor.temp.cbk",
		chronobase.Meta{Metric: "sensor.temp", Source: "edge-01"})
	if err != nil {
		panic(err)
	}
	for _, p := range points { // points 按时间戳非递减排列
		if err := w.Add(p.Timestamp, p.Value); err != nil {
			panic(err)
		}
	}
	if err := w.Close(); err != nil { // 封存并落盘全部元数据
		panic(err)
	}

	// 之后任意时刻重新打开，独立读取。
	b, err := chronobase.Open("2026-09-30T14.sensor.temp.cbk")
	if err != nil {
		panic(err)
	}
	defer b.Close()

	// 完整顺序读回。
	it := b.ReadAll()
	for it.Next() {
		p := it.Point()
		fmt.Println(p.Timestamp, p.Value)
	}
	if err := it.Err(); err != nil {
		panic(err)
	}

	// 按时间区间 [start, end) 局部读取。
	it = b.ReadRange(start, end)
	for it.Next() {
		p := it.Point()
		fmt.Println(p.Timestamp, p.Value)
	}
	if err := it.Err(); err != nil {
		panic(err)
	}
}
```

## API 概览

- `Create(path, meta) (*Writer, error)`：创建写入器，数据先写临时文件，
  `Close` 封存成功后原子替换为正式文件。
- `Writer.Add(ts int64, value float64) error`：追加一点；时间戳必须非递减
  且与块内已有点同属一个 UTC 小时。
- `Writer.Close() error`：封存块，落盘索引与文件尾。
- `Open(path) (*Block, error)`：打开封存块，仅读取头部与索引元数据。
- `Block.Meta() Meta` / `Block.NumPoints() uint64` / `Block.HourStart() int64`。
- `Block.ReadAll() *Iterator`：按写入顺序逐点产出全部数据。
- `Block.ReadRange(start, end int64) *Iterator`：逐点产出 `[start, end)`
  内的全部点，只局部读取并解压覆盖区间的压缩子块。
- `Iterator.Next() bool` / `Iterator.Point() Point` / `Iterator.Err() error`。

## 测试

```sh
go test ./...
```

覆盖：特殊浮点位型（±0、±Inf、非规格化数、不同载荷 NaN）关闭重开后的
逐位往返；长串重复时间戳两侧及内部的区间读取（含空结果与块尾部查询）；
重新打开后的局部读取行为（区间结果与直接过滤原始序列逐点一致）；
写入约束校验、空块与非法区间。
