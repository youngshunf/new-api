package system_setting

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

type ThemeSettings struct {
	Frontend string `json:"frontend"`
}

// huanxing: 默认 theme 设为 classic（classic 前端同样含 huanxing admin-tokens 等定制）。
// 已持久化 theme.frontend 的现网会通过 model/option.go:585 的 DB 加载覆盖回各自值，不影响已部署实例。
var themeSettings = ThemeSettings{
	Frontend: "classic",
}

func init() {
	config.GlobalConfig.Register("theme", &themeSettings)
	syncThemeToCommon()
}

func syncThemeToCommon() {
	common.SetTheme(themeSettings.Frontend)
}

func GetThemeSettings() *ThemeSettings {
	return &themeSettings
}

// UpdateAndSyncTheme syncs the theme config to common after DB load.
func UpdateAndSyncTheme() {
	syncThemeToCommon()
}
