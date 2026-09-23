package gatehelper

import (
	"encoding/json"
	"sync"
)

// Version 是内嵌 gate 发布集的版本（assets/stable.json 的 version）；没有制品时为空。
// 「工具配置」页据此判断主机上装的 gate 是否与本固件内嵌的同一版。
func Version() string {
	versionOnce.Do(func() {
		raw, err := files.ReadFile("assets/stable.json")
		if err != nil {
			return
		}
		var index struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(raw, &index) == nil {
			version = index.Version
		}
	})
	return version
}

var (
	versionOnce sync.Once
	version     string
)
