package bootstrap

import (
	_ "embed"
	"encoding/json"
)

//go:embed seeds.json
var seedsRaw []byte

// DefaultSeeds 返回编译期嵌入的冻结种子清单。
func DefaultSeeds() (*Seeds, error) {
	var s Seeds
	if err := json.Unmarshal(seedsRaw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
