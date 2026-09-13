package rules

import _ "embed"

// 内嵌冻结规则地板（L0，M3-W2 设计 §2.3）：编译期打包的出厂规则，
// 拔网线冷启动 / 磁盘损坏 / 验签失败时的最终兜底。版本恒为 1，
// 任何远程规则必须 version > 1 才被接受（DecideVersion 无特例分支）。
//
//go:embed embedded.json
var EmbeddedJSON []byte
