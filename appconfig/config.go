package appconfig

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
)

// Config はアプリ全体の設定（config.json から読み書きされる）
type Config struct {
	// --- キャプチャ設定 ---

	// Network はキャプチャするNICの説明文。"auto" で最アクティブなNICを自動選択。
	Network string `json:"network"`
	// AutoCheck は Network=="auto" のときサンプリングする秒数。デフォルト: 3
	AutoCheck int `json:"auto_check"`
	// Locations は locations.json のパス。デフォルト: "data/locations.json"
	Locations string `json:"locations"`

	// --- 通知設定 ---

	// DiscordWebhook は Discord の Webhook URL。空で無効。
	DiscordWebhook string `json:"discord_webhook"`
	// DebounceSeconds は同Ch+場所の重複通知を抑制する秒数。デフォルト: 30
	DebounceSeconds int `json:"debounce_seconds"`
	// ChatExclude はワールドチャット検知を抑制するキーワード一覧。
	ChatExclude []string `json:"chat_exclude"`

	// --- GUI / ADB 設定 ---

	// GUIPort はWebGUIのポート番号。0でGUI無効。デフォルト: 8080
	GUIPort int `json:"gui_port"`
	// ADBPath はadb.exeのパス。デフォルト: "adb"
	ADBPath string `json:"adb_path"`
	// MumuSerials はADBシリアル一覧。空の場合は自動検出。
	MumuSerials []string `json:"mumu_serials"`
	// MumuTapX, MumuTapY はチャンネル入力欄のタップ座標
	MumuTapX int `json:"mumu_tap_x"`
	MumuTapY int `json:"mumu_tap_y"`
	// MumuClearLength は入力前にDELを送る回数
	MumuClearLength int `json:"mumu_clear_length"`
	// MumuPreKeycode はタップ前に送るキーコード
	MumuPreKeycode string `json:"mumu_pre_keycode"`
	// MumuDelayMs は各ADBコマンド間のウェイト(ms)。デフォルト: 800
	MumuDelayMs int `json:"mumu_delay_ms"`

	// --- チャンネル巡回設定 ---

	// PatrolChannelsFile はチャンネルリストファイルのパス。デフォルト: "channels.txt"
	PatrolChannelsFile string `json:"patrol_channels_file"`

	// PatrolDwellSecs はch移動完了後〜次ch移動開始までの待機秒数。デフォルト: 60
	PatrolDwellSecs float64 `json:"patrol_dwell_secs"`

	// PatrolMoveTimeoutSecs は[0x2E]パケットを待つ最大秒数。
	// 時間内に全台分揃わなければ強制的に次へ進む。0=無効。デフォルト: 30
	PatrolMoveTimeoutSecs float64 `json:"patrol_move_timeout_secs"`

	// ParallelLimit は同時切替の最大台数。0=無制限（グループディレイも無効）。
	ParallelLimit int `json:"parallel_limit"`

	// ParallelGroupDelaySecs はグループ間の待機秒数。ParallelLimit>0のとき有効。
	ParallelGroupDelaySecs float64 `json:"parallel_group_delay_secs"`

	// PatrolSerials は巡回に使うADBシリアル一覧。空の場合は全デバイスを使用。
	PatrolSerials []string `json:"patrol_serials"`
}

func defaultConfig() *Config {
	return &Config{
		AutoCheck:              3,
		DebounceSeconds:        30,
		Locations:              "data/locations.json",
		GUIPort:                8080,
		ADBPath:                "adb",
		MumuTapX:               975,
		MumuTapY:               664,
		MumuClearLength:        3,
		MumuPreKeycode:         "KEYCODE_P",
		MumuDelayMs:            800,
		ParallelLimit:          0,
		ParallelGroupDelaySecs: 0,
		PatrolChannelsFile:     "channels.txt",
		PatrolDwellSecs:        60,
		PatrolMoveTimeoutSecs:  30,
	}
}

// Load reads config.json at path. A missing file yields defaults without error.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return defaultConfig(), nil
		}
		return nil, err
	}
	cfg := defaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.AutoCheck <= 0 {
		cfg.AutoCheck = 3
	}
	if cfg.DebounceSeconds <= 0 {
		cfg.DebounceSeconds = 30
	}
	if cfg.Locations == "" {
		cfg.Locations = "data/locations.json"
	}
	if cfg.GUIPort == 0 {
		cfg.GUIPort = 8080
	}
	if cfg.ADBPath == "" {
		cfg.ADBPath = "adb"
	}
	if cfg.MumuTapX == 0 {
		cfg.MumuTapX = 975
	}
	if cfg.MumuTapY == 0 {
		cfg.MumuTapY = 664
	}
	if cfg.MumuClearLength == 0 {
		cfg.MumuClearLength = 3
	}
	if cfg.MumuPreKeycode == "" {
		cfg.MumuPreKeycode = "KEYCODE_P"
	}
	if cfg.MumuDelayMs == 0 {
		cfg.MumuDelayMs = 800
	}
	// ParallelLimit: 0は有効値（無制限）なのでデフォルト補正しない
	// 旧フィールド parallel_group_delay_ms からの移行
	if cfg.ParallelGroupDelaySecs == 0 {
		// 0は有効値（ディレイなし）なのでそのまま
	}
	return cfg, nil
}

// Save writes cfg as indented JSON to path.
func Save(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
