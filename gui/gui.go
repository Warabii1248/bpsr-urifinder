// Package gui はローカルHTTPサーバーとして動作するWebベースGUIを提供する。
// Edge WebView2を使った専用ウィンドウで表示する。
package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	webview "github.com/jchv/go-webview2"

	"github.com/balrogsxt/StarResonanceAPI/mumu"
	"github.com/balrogsxt/StarResonanceAPI/notifier"
)

// DeviceSessionInfo はキャプチャセッション情報をGUIに渡す型
type DeviceSessionInfo struct {
	Label     string
	ClientIP  string
	UserUID   uint64
	MapID     uint32
	LineID    uint32
	Confirmed bool
}

// Server はGUI用HTTPサーバー
type Server struct {
	port               int
	mumuCfg            mumu.Config
	patroller          *mumu.Patroller
	patrolChannels     []uint32 // 起動時に設定から読み込んだチャンネルリスト
	// patrolDwellSecs は cfg.PatrolDwellSecs に移行（廃止）
	patrolChannelsFile string   // channels.txt パス（ホットリロード用）
	getSessions        func() []DeviceSessionInfo // ADB ↔ UID 対応用セッション提供コールバック
	testDetectFn       func()                     // テスト検知発火コールバック
	saveChannelsFn     func([]uint32) error        // channels.txt 保存コールバック
	getConfigFn        func() ([]byte, error)      // config.json 読み込みコールバック
	saveConfigFn       func([]byte) error          // config.json 保存コールバック
	// cfgUpdaterFn は config.json 保存後に呼ばれ、時間系設定を Patroller にリアルタイム反映する
	cfgUpdaterFn       func(dwellSecs, moveTimeoutSecs, groupDelaySecs float64, parallelLimit int)

	cfgMu    sync.RWMutex // mumuCfg 専用ミューテックス
	mu       sync.RWMutex
	logLines       []string      // 検知ログ（最大200件）
	clients        []chan string // SSEクライアント
	goldBoarHistory []GoldBoarEvent // 金ウリボ検知履歴（最大50件）
}

// GoldBoarEvent は金ウリボ検知の1件分の記録
type GoldBoarEvent struct {
	Time     string `json:"time"`
	Channel  uint32 `json:"channel"`
	Location string `json:"location"`
}

// getMumuCfg は mumuCfg をスレッドセーフに取得する
func (s *Server) getMumuCfg() mumu.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.mumuCfg
}

// setMumuCfg は mumuCfg をスレッドセーフに更新する
func (s *Server) setMumuCfg(cfg mumu.Config) {
	s.cfgMu.Lock()
	s.mumuCfg = cfg
	s.cfgMu.Unlock()
}

// New はGUIサーバーを作成する
func New(port int, mumuCfg mumu.Config, patrolChannels []uint32, patrolChannelsFile string) *Server {
	return &Server{
		port:               port,
		mumuCfg:            mumuCfg,
		patroller:          mumu.NewPatroller(mumuCfg),
		patrolChannels:     patrolChannels,
		patrolChannelsFile: patrolChannelsFile,
	}
}

// SetSessionProvider はADD ↔ UID 対応に使うセッション情報提供関数を設定する。
func (s *Server) SetSessionProvider(fn func() []DeviceSessionInfo) {
	s.getSessions = fn
}

// SetTestDetectFn はテスト通知ボタンから呼ばれるコールバックを設定する。
func (s *Server) SetTestDetectFn(fn func()) {
	s.testDetectFn = fn
}

// SetSaveChannelsFn はチャンネルリスト保存コールバックを設定する。
func (s *Server) SetSaveChannelsFn(fn func([]uint32) error) {
	s.saveChannelsFn = fn
}

// SetConfigFns は config.json の読み書きコールバックを設定する。
func (s *Server) SetConfigFns(getFn func() ([]byte, error), saveFn func([]byte) error) {
	s.getConfigFn = getFn
	s.saveConfigFn = saveFn
}

// SetCfgUpdater は config.json 保存後に時間系設定を Patroller へリアルタイム反映するコールバックを設定する。
func (s *Server) SetCfgUpdater(fn func(dwellSecs, moveTimeoutSecs, groupDelaySecs float64, parallelLimit int)) {
	s.cfgUpdaterFn = fn
}

// NotifyChMovePacket は ncap が [0x2E] パケットを受信したとき main.go から呼ぶ。
// 巡回中であれば Patroller に移動完了シグナルを転送する。
func (s *Server) NotifyChMovePacket() {
	s.patroller.NotifyChMovePacket()
}

// UpdatePatrollerCfg は Patroller の設定をリアルタイムで更新する。
// SetCfgUpdater コールバック内から呼ぶ。
func (s *Server) UpdatePatrollerCfg(cfg mumu.Config) {
	s.setMumuCfg(cfg)
	s.patroller.UpdateConfig(cfg)
}

// OnDetect は検知イベントをGUIのログに追加するコールバック
func (s *Server) OnDetect(det notifier.Detection) {
	line := fmt.Sprintf("[%s] %s", det.Time.Format("15:04:05"), notifier.Format(det))
	detCh := det.LineID // 検知されたチャンネル番号

	s.mu.Lock()
	// 通常ログに追記
	s.logLines = append(s.logLines, line)
	if len(s.logLines) > 200 {
		s.logLines = s.logLines[len(s.logLines)-200:]
	}
	// 金ウリボ履歴に追記（最大50件）
	event := GoldBoarEvent{
		Time:     det.Time.Format("01/02 15:04:05"),
		Channel:  detCh,
		Location: notifier.Format(det),
	}
	s.goldBoarHistory = append(s.goldBoarHistory, event)
	if len(s.goldBoarHistory) > 50 {
		s.goldBoarHistory = s.goldBoarHistory[len(s.goldBoarHistory)-50:]
	}
	// 巡回チャンネルリストから該当chを削除
	newChs := make([]uint32, 0, len(s.patrolChannels))
	removed := false
	for _, pc := range s.patrolChannels {
		if pc == detCh {
			removed = true
			continue
		}
		newChs = append(newChs, pc)
	}
	if removed {
		s.patrolChannels = newChs
		log.Printf("[GUI] 金ウリボ検知: Ch%d を巡回リストから削除 (残%d ch)", detCh, len(newChs))
	}
	saveChannelsFn := s.saveChannelsFn
	clients := make([]chan string, len(s.clients))
	copy(clients, s.clients)
	s.mu.Unlock()

	// ファイルに保存（ロック外）
	if removed && saveChannelsFn != nil {
		if err := saveChannelsFn(newChs); err != nil {
			log.Printf("[GUI] channels.txt 保存失敗: %v", err)
		}
	}
	// SSEで全クライアントに通知
	for _, ch := range clients {
		select {
		case ch <- line:
		default:
		}
	}
}

// AddLog は1行のログをGUIのSSEストリームとlogLinesに追加する
func (s *Server) AddLog(line string) {
	s.mu.Lock()
	s.logLines = append(s.logLines, line)
	if len(s.logLines) > 200 {
		s.logLines = s.logLines[len(s.logLines)-200:]
	}
	clients := make([]chan string, len(s.clients))
	copy(clients, s.clients)
	s.mu.Unlock()

	for _, ch := range clients {
		select {
		case ch <- line:
		default:
		}
	}
}

// guiWriter は標準 log 出力をGUIのSSEにも転送する io.Writer
type guiWriter struct {
	base io.Writer
	srv  *Server
	buf  []byte
}

func (w *guiWriter) Write(p []byte) (int, error) {
	n, err := w.base.Write(p)
	w.buf = append(w.buf, p...)
	for {
		idx := -1
		for i, b := range w.buf {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:idx]), "\r")
		w.buf = w.buf[idx+1:]
		if line != "" {
			w.srv.AddLog(line)
			// [0x2E] パケットログを検知して巡回の移動完了シグナルを送る
			if strings.Contains(line, "[0x2E]") {
				w.srv.patroller.NotifyChMovePacket()
			}
		}
	}
	return n, err
}

// LogWriter は log.SetOutput() に渡す io.Writer を返す。
func (s *Server) LogWriter(base io.Writer) io.Writer {
	return &guiWriter{base: base, srv: s}
}

// startHTTP はHTTPサーバーをバックグラウンドで起動する（内部用）
func (s *Server) startHTTP(ctx context.Context) (string, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/devices", s.handleDevices)
	mux.HandleFunc("/api/device-map", s.handleDeviceMap)
	mux.HandleFunc("/api/switch", s.handleSwitch)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/patrol/start", s.handlePatrolStart)
	mux.HandleFunc("/api/patrol/stop", s.handlePatrolStop)
	mux.HandleFunc("/api/patrol/status", s.handlePatrolStatus)
	mux.HandleFunc("/api/patrol/channels", s.handlePatrolChannels)
	mux.HandleFunc("/api/test-detect", s.handleTestDetect)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/gold-history", s.handleGoldHistory)
	mux.HandleFunc("/events", s.handleSSE)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("GUI server listen: %w", err)
	}

	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
			log.Printf("[GUI] HTTP server error: %v", err)
		}
	}()

	// 起動時にデバイス一覧を取得してログに出力する（再起動あり）
	go func() {
		time.Sleep(1 * time.Second)
		log.Println("[MuMu] 起動時デバイス確認...")
		devices, err := mumu.ListDevices(s.getMumuCfg())
		if err != nil {
			log.Printf("[MuMu] 起動時デバイス取得失敗: %v", err)
			return
		}
		if len(devices) == 0 {
			log.Println("[MuMu] 起動時デバイスが見つかりません。MuMu Playerを起動してadb connectで接続してください")
		} else {
			log.Printf("[MuMu] 起動時デバイス: %v", devices)
		}
	}()

	url := fmt.Sprintf("http://%s", ln.Addr().String())
	return url, nil
}

// RunWindow はHTTPサーバーを起動しEdge WebView2の専用ウィンドウを開く。
func (s *Server) RunWindow(ctx context.Context) error {
	url, err := s.startHTTP(ctx)
	if err != nil {
		return err
	}
	log.Printf("[GUI] opening window: %s", url)

	w := webview.NewWithOptions(webview.WebViewOptions{
		Debug: false,
		WindowOptions: webview.WindowOptions{
			Title:  "LoyalBoarlet Monitor",
			Width:  1000,
			Height: 720,
			Center: true,
		},
	})
	if w == nil {
		log.Println("[GUI] WebView2 unavailable, falling back to browser")
		openBrowser(url)
		<-ctx.Done()
		return nil
	}
	defer w.Destroy()
	w.Navigate(url)
	w.Run()
	return nil
}

// Start はHTTPサーバーをバックグラウンド起動してブラウザで開く
func (s *Server) Start(ctx context.Context) error {
	url, err := s.startHTTP(ctx)
	if err != nil {
		return err
	}
	log.Printf("[GUI] http server: %s", url)
	openBrowser(url)
	<-ctx.Done()
	return nil
}

func openBrowser(url string) {
	cmd := exec.Command("cmd", "/c", "start", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		log.Printf("[GUI] browser open failed: %v", err)
	}
}

// handleIndex はメインHTMLページを返す
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, indexHTML)
}

// handleDeviceMap はADBデバイスのエミュレータIPを取得し、
// キャプチャセッション（UID等）と紐付けた一覧をJSONで返す。
func (s *Server) handleDeviceMap(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	adbDevices, err := mumu.ListDevices(s.getMumuCfg())
	if err != nil {
		log.Printf("[MuMu] device-map: ListDevices error: %v", err)
		http.Error(w, err.Error(), 500)
		return
	}

	ipToSess := make(map[string]DeviceSessionInfo)
	if s.getSessions != nil {
		for _, sess := range s.getSessions() {
			if sess.ClientIP != "" {
				ipToSess[sess.ClientIP] = sess
			}
		}
	}

	type DeviceEntry struct {
		Serial    string `json:"serial"`
		DeviceIP  string `json:"device_ip"`
		UserUID   uint64 `json:"user_uid"`
		Label     string `json:"label"`
		MapID     uint32 `json:"map_id"`
		LineID    uint32 `json:"line_id"`
		Confirmed bool   `json:"confirmed"`
	}

	entries := make([]DeviceEntry, len(adbDevices))
	var wg sync.WaitGroup
	for i, serial := range adbDevices {
		entries[i] = DeviceEntry{Serial: serial}
		wg.Add(1)
		go func(idx int, ser string) {
			defer wg.Done()
			ipCh := make(chan string, 1)
			go func() {
				ip, ipErr := mumu.GetDeviceIP(ser, s.getMumuCfg())
				if ipErr != nil {
					log.Printf("[MuMu] GetDeviceIP %s: %v", ser, ipErr)
					ip = ""
				}
				ipCh <- ip
			}()
			var devIP string
			select {
			case devIP = <-ipCh:
			case <-ctx.Done():
			}
			entries[idx].DeviceIP = devIP
			if devIP != "" {
				if sess, ok := ipToSess[devIP]; ok {
					entries[idx].UserUID = sess.UserUID
					entries[idx].Label = sess.Label
					entries[idx].MapID = sess.MapID
					entries[idx].LineID = sess.LineID
					entries[idx].Confirmed = sess.Confirmed
				}
			}
		}(i, serial)
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"devices": entries})
}

// handleDevices はADB接続デバイス一覧をJSONで返す
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := mumu.ListDevices(s.getMumuCfg())
	if err != nil {
		log.Printf("[MuMu] adb devices エラー: %v", err)
	}
	if devices == nil {
		devices = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"devices": devices,
	})
}

// handleSwitch はチャンネル切替リクエストを処理する
func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Serial  string `json:"serial"`
		Channel uint32 `json:"channel"`
		All     bool   `json:"all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	type result struct {
		Serial string `json:"serial"`
		Error  string `json:"error,omitempty"`
		OK     bool   `json:"ok"`
	}
	var results []result

	if req.All {
		serials, err := mumu.ListDevices(s.getMumuCfg())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		for serial, err := range mumu.SwitchAll(serials, req.Channel, s.getMumuCfg()) {
			r := result{Serial: serial, OK: err == nil}
			if err != nil {
				r.Error = err.Error()
			}
			results = append(results, r)
		}
	} else {
		err := mumu.SwitchChannel(req.Serial, req.Channel, s.getMumuCfg())
		r := result{Serial: req.Serial, OK: err == nil}
		if err != nil {
			r.Error = err.Error()
		}
		results = append(results, r)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"results": results,
	})
}

// handleLogs は既存ログ一覧をJSONで返す
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	lines := make([]string, len(s.logLines))
	copy(lines, s.logLines)
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"logs": lines,
	})
}

// handlePatrolChannels はconfig読み込み済みのチャンネルリストを返す（GET）または保存する（POST）
func (s *Server) handlePatrolChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Channels []uint32 `json:"channels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.patrolChannels = req.Channels
		if s.saveChannelsFn != nil {
			if err := s.saveChannelsFn(req.Channels); err != nil {
				log.Printf("[GUI] channels保存失敗: %v", err)
				http.Error(w, "save failed: "+err.Error(), 500)
				return
			}
			log.Printf("[GUI] channels.txt に %d件保存しました", len(req.Channels))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"channels": s.patrolChannels,
	})
}

// handleGoldHistory は金ウリボ検知履歴を返す
func (s *Server) handleGoldHistory(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	h := make([]GoldBoarEvent, len(s.goldBoarHistory))
	copy(h, s.goldBoarHistory)
	s.mu.RUnlock()
	// 新しい順に返す
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h)
}

// handleConfig は config.json の読み込み（GET）または保存（POST）を行う。
// 保存後は再起動で反映される。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if s.saveConfigFn == nil {
			http.Error(w, "save not configured", 503)
			return
		}
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := s.saveConfigFn(buf); err != nil {
			log.Printf("[GUI] config保存失敗: %v", err)
			http.Error(w, "save failed: "+err.Error(), 500)
			return
		}
		log.Printf("[GUI] config.json を保存しました")

		// 時間系設定をPatrollerにリアルタイム反映
		if s.cfgUpdaterFn != nil {
			var raw map[string]json.RawMessage
			if json.Unmarshal(buf, &raw) == nil {
				getF64 := func(key string, def float64) float64 {
					if v, ok := raw[key]; ok {
						var f float64
						if json.Unmarshal(v, &f) == nil {
							return f
						}
					}
					return def
				}
				getInt := func(key string, def int) int {
					if v, ok := raw[key]; ok {
						var i int
						if json.Unmarshal(v, &i) == nil {
							return i
						}
					}
					return def
				}
				dwellSecs       := getF64("patrol_dwell_secs", 60)
				moveTimeoutSecs := getF64("patrol_move_timeout_secs", 30)
				groupDelaySecs  := getF64("parallel_group_delay_secs", 0)
				parallelLimit   := getInt("parallel_limit", 0)
				s.cfgUpdaterFn(dwellSecs, moveTimeoutSecs, groupDelaySecs, parallelLimit)
				log.Printf("[GUI] 巡回設定を即時反映: 滞在=%.0fs, タイムアウト=%.0fs, グループ間=%.0fs, 並列=%d",
					dwellSecs, moveTimeoutSecs, groupDelaySecs, parallelLimit)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
		return
	}
	if s.getConfigFn == nil {
		http.Error(w, "config not available", 503)
		return
	}
	data, err := s.getConfigFn()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handlePatrolStart は巡回を開始する
func (s *Server) handlePatrolStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Serials      []string `json:"serials"`
		Channels     []uint32 `json:"channels"`
		Reversed     bool     `json:"reversed"`
		LoopMode     bool     `json:"loop_mode"`
		StartChannel uint32   `json:"start_channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	channels := req.Channels
	if len(channels) == 0 {
		channels = s.patrolChannels
	}
	opts := mumu.PatrolOptions{
		Reversed:     req.Reversed,
		LoopMode:     req.LoopMode,
		StartChannel: req.StartChannel,
	}
	s.patroller.Start(req.Serials, channels, s.patrolChannelsFile, opts)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// handleTestDetect はテスト用ゴールドウリボ検知を発火する
func (s *Server) handleTestDetect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	if s.testDetectFn == nil {
		http.Error(w, "test detect not configured", 503)
		return
	}
	go s.testDetectFn()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// handlePatrolStop は巡回を停止する
func (s *Server) handlePatrolStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	s.patroller.Stop()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// handlePatrolStatus は現在の巡回状態を返す
func (s *Server) handlePatrolStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.patroller.Status())
}

// handleSSE はServer-Sent Eventsで検知ログをリアルタイム配信する
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 32)
	s.mu.Lock()
	s.clients = append(s.clients, ch)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		for i, c := range s.clients {
			if c == ch {
				s.clients = append(s.clients[:i], s.clients[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()

	ctx := r.Context()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			escaped := strings.ReplaceAll(msg, "\n", "\\n")
			fmt.Fprintf(w, "data: %s\n\n", escaped)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

const indexHTML = `<!DOCTYPE html>
<html lang="ja">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>LoyalBoarlet Monitor</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#0a0a1a;color:#eaeaea;font-family:'Segoe UI',sans-serif;font-size:14px;height:100vh;width:100vw;overflow:hidden;user-select:none;display:flex;flex-direction:column}
h1{font-size:1.1em;padding:6px 12px;background:#0d1b33;border-bottom:1px solid #1a3a6a;display:flex;align-items:center;gap:8px;height:36px;flex-shrink:0}
button{background:#1a3a6a;color:#eaeaea;border:none;padding:6px 14px;border-radius:4px;cursor:pointer;font-size:0.88em;transition:background .15s}
button:hover{background:#2a5aa0}
button:disabled{opacity:.4;cursor:default}
button.green{background:#1b5e20}
button.green:hover{background:#2e7d32}
button.secondary{background:#1a2a3a}
button.secondary:hover{background:#2a3a4a}
button.toggle-btn{padding:5px 12px;font-size:0.85em}
button.toggle-btn.active{background:#1565c0}
button.toggle-btn.active:hover{background:#1976d2}
input[type=text],input[type=number],textarea,select{background:#0f3460;color:#eaeaea;border:1px solid #334466;border-radius:4px;padding:5px 8px;font-size:0.88em}
input[type=checkbox]{accent-color:#e94560;width:16px;height:16px}
.flex-row{display:flex;flex-wrap:wrap;gap:8px;align-items:center}
/* ─── Layout ─── */
#workspace{display:flex;flex-direction:row;flex:1;overflow:hidden;min-height:0;width:100%}
.panel-col{display:flex;flex-direction:column;overflow:hidden;min-width:200px;min-height:0}
.splitter-h{width:5px;background:#1a3a6a;cursor:col-resize;flex-shrink:0}
.splitter-h:hover,.splitter-h.active{background:#2a7ae0}
/* ─── Panel ─── */
.panel{display:flex;flex-direction:column;background:#0d1b33;border:1px solid #1a3a6a;border-radius:6px;margin:3px;overflow:hidden;transition:flex .15s}
.panel.minimized{flex:none!important}
.panel-header{display:flex;align-items:center;gap:6px;padding:5px 10px;background:#111e38;border-bottom:1px solid #1a3a6a;cursor:grab;flex-shrink:0;height:32px;border-radius:5px 5px 0 0}
.panel-header:active{cursor:grabbing}
.panel.minimized .panel-header{border-bottom:none;border-radius:5px}
.panel-title{font-size:0.85em;font-weight:bold;flex:1;pointer-events:none;white-space:nowrap}
.panel-btn{background:none;border:none;color:#aaa;cursor:pointer;padding:2px 6px;font-size:1em;border-radius:3px;line-height:1}
.panel-btn:hover{background:#2a3a5a;color:#fff}
.panel-body{flex:1;overflow-y:auto;padding:10px;min-height:0}
.panel.minimized .panel-body{display:none!important}
/* Drop indicator */
.drop-indicator{display:none;position:fixed;background:rgba(42,122,224,.3);border:2px dashed #2a7ae0;border-radius:4px;pointer-events:none;z-index:999}
.drop-indicator.visible{display:block}
/* Content */
.device-list{display:flex;flex-direction:column;gap:6px;margin-top:8px}
.device-entry{background:#0d0d1a;border-radius:6px;padding:8px 10px}
.device-entry .serial{color:#7ec8e3;font-family:monospace;font-size:0.85em}
.device-entry .uid{color:#a0a0b0;font-size:0.8em}
.device-entry.matched .uid{color:#ffd700}
.no-devices{color:#606080;font-size:0.85em;padding:4px 0}
.log-area{flex:1;background:#0a0a14;overflow-y:auto;padding:8px;font-family:monospace;font-size:0.8em;min-height:0}
.log-line{color:#b0b0c0;padding:1px 0;white-space:pre-wrap;word-break:break-all}
.log-line.detect{color:#ffd700;font-weight:bold}
#status-bar{color:#4caf50;font-size:0.82em}
.patrol-status{background:#090f20;border-radius:4px;padding:6px 10px;font-size:0.82em;margin-bottom:6px}
.patrol-status span{margin-right:12px}
.patrol-status .running{color:#4caf50;font-weight:bold}
.patrol-status .stopped{color:#888}
.ch-editor{display:flex;flex-direction:column;gap:4px;max-height:150px;overflow-y:auto;margin-bottom:6px}
.ch-row{display:flex;gap:6px;align-items:center;background:#0d0d1a;border-radius:4px;padding:4px 8px}
.ch-row .ch-num{color:#7ec8e3;font-family:monospace;font-size:0.9em;min-width:24px;text-align:right}
.cfg-grid{display:grid;grid-template-columns:1fr 1fr;gap:10px 20px}
.cfg-field{display:flex;flex-direction:column;gap:3px}
.cfg-field label{color:#a0a0b0;font-size:0.82em}
.cfg-field input{width:100%}
.cfg-save-bar{display:flex;gap:8px;align-items:center;margin-top:10px}
.cfg-note{font-size:0.75em;color:#606080;margin-top:4px}
.check-label{display:flex;align-items:center;gap:6px;cursor:pointer;color:#eaeaea}
.section-title{font-size:0.8em;color:#7ec8e3;font-weight:bold;margin:10px 0 4px;border-bottom:1px solid #1a3a6a;padding-bottom:3px}
.gold-table{width:100%;border-collapse:collapse;font-size:0.82em}
.gold-table th{color:#ffd700;text-align:left;padding:3px 8px;border-bottom:1px solid #1a3a6a;white-space:nowrap}
.gold-table td{padding:4px 8px;border-bottom:1px solid #0d1530;vertical-align:top}
.gold-table tr:hover td{background:#0d1b33}
.gold-table .ch-cell{color:#7ec8e3;font-family:monospace;font-weight:bold;white-space:nowrap}
.gold-table .time-cell{color:#a0a0b0;white-space:nowrap;font-size:0.85em}
.no-history{color:#606080;font-size:0.85em;padding:8px 0}
</style>
</head>
<body>
<h1>🐗 LoyalBoarlet Monitor</h1>
<div id="workspace">
  <!-- 左カラム -->
  <div class="panel-col" id="col-left" style="flex:1.3">
    <div class="panel" id="panel-devices">
      <div class="panel-header" draggable="true" data-panel="panel-devices">
        <span class="panel-title">📱 デバイス一覧 &amp; 手動切替</span>
        <button class="panel-btn" onclick="minimizePanel('panel-devices')" title="最小化">─</button>
      </div>
      <div class="panel-body">
        <div class="flex-row">
          <button onclick="refreshDevices()">🔄 再取得</button>
          <label>一括 Ch:</label>
          <input type="number" id="allch" min="1" max="999" value="1" style="width:65px">
          <button onclick="switchAll()">▶ 全切替</button>
          <span id="status-bar"></span>
        </div>
        <div class="device-list" id="device-list"><div class="no-devices">読み込み中...</div></div>
      </div>
    </div>
    <div class="panel" id="panel-patrol" style="flex:2">
      <div class="panel-header" draggable="true" data-panel="panel-patrol">
        <span class="panel-title">🔁 チャンネル巡回</span>
        <button class="panel-btn" onclick="minimizePanel('panel-patrol')" title="最小化">─</button>
      </div>
      <div class="panel-body">
        <div class="patrol-status">
          <span class="stopped" id="ps-state">■ 停止中</span>
          <span id="ps-ch"></span>
          <span id="ps-prog"></span>
          <span id="ps-parallel"></span>
        </div>
        <div id="ps-full" style="font-size:0.78em;color:#e57373;min-height:1em;margin-bottom:6px"></div>
        <div class="flex-row" style="margin-bottom:8px">
          <label>開始Ch:</label>
          <input type="number" id="patrol-start-ch" min="0" max="9999" value="0" style="width:65px" title="0=前回位置から再開">
          <button class="secondary toggle-btn" id="btn-reversed" onclick="toggleReversed()">⬆ 正順</button>
          <button class="secondary toggle-btn" id="btn-loop" onclick="toggleLoop()">🔁 ループ</button>
        </div>
        <div class="flex-row" style="margin-bottom:8px">
          <button class="green" id="btn-patrol-start" onclick="patrolStart()">▶ 巡回開始</button>
          <button class="secondary" id="btn-patrol-stop" onclick="patrolStop()" disabled>■ 停止</button>
        </div>
        <div class="section-title">巡回チャンネル</div>
        <div style="display:flex;gap:6px;flex-wrap:wrap;margin-bottom:6px">
          <button class="secondary" style="padding:3px 8px;font-size:0.8em" onclick="addChannel()">＋ 追加</button>
          <button class="secondary" style="padding:3px 8px;font-size:0.8em" onclick="sortChannels('asc')">↑ 昇順</button>
          <button class="secondary" style="padding:3px 8px;font-size:0.8em" onclick="sortChannels('desc')">↓ 降順</button>
          <button class="secondary" style="padding:3px 8px;font-size:0.8em" id="btn-ch-save" onclick="saveChannels()" disabled>💾 保存</button>
          <span id="ch-save-status" style="font-size:0.8em;color:#a0a0b0"></span>
        </div>
        <div class="ch-editor" id="ch-editor"><div class="no-devices">読み込み中...</div></div>
        <div style="display:flex;gap:6px;align-items:center;margin-top:6px">
          <input type="text" id="ch-bulk-input" placeholder="例: 6,13,23,35,41..." style="flex:1;width:auto;font-size:0.85em">
          <button class="secondary" style="padding:3px 10px;font-size:0.8em;white-space:nowrap" onclick="bulkImportChannels()">上書き</button>
        </div>
      </div>
    </div>
  </div>

  <div class="splitter-h" id="splitter-main"></div>

  <!-- 右カラム -->
  <div class="panel-col" id="col-right" style="flex:1">
    <div class="panel" id="panel-gold">
      <div class="panel-header" draggable="true" data-panel="panel-gold">
        <span class="panel-title">🌟 金ウリボ検知履歴</span>
        <button class="panel-btn" onclick="minimizePanel('panel-gold')" title="最小化">─</button>
      </div>
      <div class="panel-body">
        <div id="gold-history-container"><div class="no-history">検知履歴なし</div></div>
      </div>
    </div>
    <div class="panel" id="panel-log" style="flex:2">
      <div class="panel-header" draggable="true" data-panel="panel-log">
        <span class="panel-title">📋 検知ログ</span>
        <button class="panel-btn" onclick="minimizePanel('panel-log')" title="最小化">─</button>
      </div>
      <div class="panel-body" style="padding:0;display:flex;flex-direction:column">
        <div class="log-area" id="log-area"></div>
        <div style="padding:6px 10px;border-top:1px solid #1a3a6a;display:flex;gap:8px;flex-shrink:0">
          <button class="secondary" style="font-size:0.8em;padding:3px 10px" onclick="document.getElementById('log-area').innerHTML=''">クリア</button>
          <button style="font-size:0.8em;padding:3px 10px" onclick="testDetect()">🔔 テスト通知</button>
        </div>
      </div>
    </div>
    <div class="panel" id="panel-config">
      <div class="panel-header" draggable="true" data-panel="panel-config">
        <span class="panel-title">⚙ 設定</span>
        <button class="panel-btn" onclick="minimizePanel('panel-config')" title="最小化">─</button>
      </div>
      <div class="panel-body">
        <div id="cfg-form" class="cfg-grid"></div>
        <div class="cfg-save-bar">
          <button onclick="saveConfig()">💾 保存</button>
          <span id="cfg-status" style="font-size:0.82em;color:#a0a0b0"></span>
        </div>
        <p class="cfg-note">* 滞在時間・タイムアウト・並列設定は保存後すぐ反映されます。その他は再起動が必要です。</p>
      </div>
    </div>
  </div>
</div>
<div class="drop-indicator" id="drop-indicator"></div>

<script>
// ── Minimize ──
function minimizePanel(id) {
  const p = document.getElementById(id);
  const btn = p.querySelector('.panel-btn');
  if (p.classList.toggle('minimized')) { btn.textContent='＋'; btn.title='展開'; }
  else { btn.textContent='─'; btn.title='最小化'; }
  saveLayout();
}

// ── Layout persistence ──
const LAYOUT_KEY = 'loyalboarlet_layout';
function saveLayout() {
  const L = document.getElementById('col-left');
  const R = document.getElementById('col-right');
  const totalW = L.getBoundingClientRect().width + R.getBoundingClientRect().width;
  const leftRatio = totalW > 0 ? L.getBoundingClientRect().width / totalW : 0.57;
  const layout = {
    leftRatio,
    left:  [...L.querySelectorAll('.panel')].map(p=>({id:p.id, minimized:p.classList.contains('minimized')})),
    right: [...R.querySelectorAll('.panel')].map(p=>({id:p.id, minimized:p.classList.contains('minimized')})),
  };
  try { localStorage.setItem(LAYOUT_KEY, JSON.stringify(layout)); } catch(_){}
}
function restoreLayout() {
  let layout;
  try { layout = JSON.parse(localStorage.getItem(LAYOUT_KEY)||'null'); } catch(_){}
  if (!layout) return;
  const L = document.getElementById('col-left');
  const R = document.getElementById('col-right');
  // カラム幅比率を復元
  if (layout.leftRatio) {
    L.style.flex = layout.leftRatio + ' 1 0';
    R.style.flex = (1 - layout.leftRatio) + ' 1 0';
  }
  // パネルの順番・最小化状態を復元
  [[L, layout.left],[R, layout.right]].forEach(([col, entries])=>{
    if (!entries) return;
    entries.forEach(({id, minimized})=>{
      const panel = document.getElementById(id);
      if (!panel) return;
      col.appendChild(panel); // 順番通りに付け直す
      const btn = panel.querySelector('.panel-btn');
      if (minimized) {
        panel.classList.add('minimized');
        if (btn) { btn.textContent='＋'; btn.title='展開'; }
      } else {
        panel.classList.remove('minimized');
        if (btn) { btn.textContent='─'; btn.title='最小化'; }
      }
    });
  });
}

// ── Splitter ──
(function(){
  const sp = document.getElementById('splitter-main');
  const L = document.getElementById('col-left');
  const R = document.getElementById('col-right');
  let drag=false,sx=0,slw=0,srw=0;
  sp.addEventListener('mousedown', e=>{
    drag=true; sx=e.clientX;
    slw=L.getBoundingClientRect().width;
    srw=R.getBoundingClientRect().width;
    sp.classList.add('active');
    document.body.style.cursor='col-resize';
    e.preventDefault();
  });
  document.addEventListener('mousemove', e=>{
    if(!drag) return;
    const dx=e.clientX-sx, total=slw+srw;
    const nl=Math.max(180,Math.min(total-180,slw+dx));
    const nr=total-nl;
    const ratio=nl/total;
    L.style.flex=ratio+' 1 0';
    R.style.flex=(1-ratio)+' 1 0';
    L.style.width=''; R.style.width='';
  });
  document.addEventListener('mouseup', ()=>{
    if(!drag) return;
    drag=false; sp.classList.remove('active');
    document.body.style.cursor='';
    saveLayout();
  });
})();

// ── Drag & Drop panels ──
let dragPanel=null;
const dropInd=document.getElementById('drop-indicator');
document.querySelectorAll('.panel-header[draggable]').forEach(h=>{
  h.addEventListener('dragstart', e=>{
    dragPanel=document.getElementById(h.dataset.panel);
    e.dataTransfer.effectAllowed='move';
    e.dataTransfer.setData('text/plain',h.dataset.panel);
    setTimeout(()=>{ if(dragPanel) dragPanel.style.opacity='0.4'; },0);
  });
  h.addEventListener('dragend', ()=>{
    if(dragPanel) dragPanel.style.opacity='';
    dragPanel=null;
    dropInd.classList.remove('visible');
    saveLayout();
  });
});
document.querySelectorAll('.panel').forEach(p=>{
  p.addEventListener('dragover', e=>{
    if(!dragPanel||p===dragPanel) return;
    e.preventDefault();
    const r=p.getBoundingClientRect();
    const before=e.clientY<r.top+r.height/2;
    dropInd.style.left=r.left+'px'; dropInd.style.width=r.width+'px';
    dropInd.style.height='3px'; dropInd.style.top=((before?r.top:r.bottom)-2)+'px';
    dropInd.classList.add('visible');
    p._dropBefore=before;
  });
  p.addEventListener('dragleave',()=>dropInd.classList.remove('visible'));
  p.addEventListener('drop', e=>{
    e.preventDefault();
    dropInd.classList.remove('visible');
    if(!dragPanel||p===dragPanel) return;
    p.parentNode.insertBefore(dragPanel, p._dropBefore?p:p.nextSibling);
  });
});
['col-left','col-right'].forEach(id=>{
  const col=document.getElementById(id);
  col.addEventListener('dragover', e=>{
    if(!dragPanel||dragPanel.closest('.panel-col')===col) return;
    e.preventDefault();
    const r=col.getBoundingClientRect();
    dropInd.style.left=r.left+'px'; dropInd.style.width=r.width+'px';
    dropInd.style.height=r.height+'px'; dropInd.style.top=r.top+'px';
    dropInd.classList.add('visible');
  });
  col.addEventListener('drop', e=>{
    e.preventDefault();
    dropInd.classList.remove('visible');
    if(!dragPanel||dragPanel.closest('.panel-col')===col) return;
    col.appendChild(dragPanel);
  });
});

// ── Devices ──
let selectedDevices=new Set();
function selectedSerials(){ return [...selectedDevices]; }
async function refreshDevices(){
  const r=await fetch('/api/devices');
  const devs=await r.json();
  const map=await fetch('/api/device-map').then(r=>r.json()).catch(()=>({}));
  const el=document.getElementById('device-list');
  if(!devs||devs.length===0){ el.innerHTML='<div class="no-devices">デバイスが見つかりません</div>'; return; }
  el.innerHTML=devs.map(d=>{
    const info=map[d]||{};
    const uid=info.user_uid||'', ch=info.line_id||'', confirmed=info.confirmed||false;
    const checked=selectedDevices.has(d)?'checked':'';
    const uidHtml=uid?('<span class="uid">'+(confirmed?'🔗':'')+' UID:'+uid+(ch?' Ch'+ch:'')+'</span>'):'';
    return '<div class="device-entry'+(confirmed?' matched':'')+'">'
      +'<label class="check-label">'
      +'<input type="checkbox" '+checked+' onchange="toggleDevice(\''+d+'\',this.checked)">'
      +'<span class="serial">'+d+'</span>'+uidHtml
      +'</label>'
      +'<div style="display:flex;gap:6px;margin-top:4px">'
      +'<input type="number" id="ch-'+d+'" min="1" max="999" value="1" style="width:65px">'
      +'<button style="padding:3px 8px;font-size:0.8em" onclick="switchOne(\''+d+'\')">切替</button>'
      +'</div></div>';
  }).join('');
}
function toggleDevice(s,c){ c?selectedDevices.add(s):selectedDevices.delete(s); }
async function switchAll(){
  const ch=document.getElementById('allch').value;
  const bar=document.getElementById('status-bar');
  bar.textContent='切替中...';
  const r=await fetch('/api/switch',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channel:parseInt(ch),serials:selectedSerials()})});
  const d=await r.json();
  bar.textContent=d.ok?'✓ 完了':'✗ '+(d.error||'失敗');
  setTimeout(()=>bar.textContent='',3000);
}
async function switchOne(serial){
  const ch=parseInt(document.getElementById('ch-'+serial).value);
  const bar=document.getElementById('status-bar');
  bar.textContent='切替中...';
  const r=await fetch('/api/switch',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channel:ch,serial})});
  const d=await r.json();
  bar.textContent=d.ok?'✓ 完了':'✗ '+(d.error||'失敗');
  setTimeout(()=>bar.textContent='',3000);
}

// ── Log / SSE ──
function appendLog(line){
  const la=document.getElementById('log-area');
  const div=document.createElement('div');
  div.className='log-line'+(line.includes('[DETECTION]')||line.includes('金')?' detect':'');
  div.textContent=line;
  la.appendChild(div);
  la.scrollTop=la.scrollHeight;
}
async function testDetect(){ await fetch('/api/test-detect',{method:'POST'}); }
(function(){
  const src=new EventSource('/events');
  src.onmessage=e=>{
    appendLog(e.data);
    if(e.data.includes('[GUI] 金ウリボ')||e.data.includes('[DETECTION]')){
      loadGoldHistory(); loadPatrolChannels();
    }
  };
  fetch('/api/logs').then(r=>r.json()).then(lines=>(lines||[]).forEach(appendLog));
})();

// ── Gold History ──
async function loadGoldHistory(){
  try{
    const h=await fetch('/api/gold-history').then(r=>r.json());
    const c=document.getElementById('gold-history-container');
    if(!h||h.length===0){ c.innerHTML='<div class="no-history">検知履歴なし</div>'; return; }
    c.innerHTML='<table class="gold-table"><thead><tr><th>時刻</th><th>Ch</th><th>場所</th></tr></thead><tbody>'
      +h.map(e=>'<tr><td class="time-cell">'+e.time+'</td><td class="ch-cell">Ch'+e.channel+'</td><td>'+e.location+'</td></tr>').join('')
      +'</tbody></table>';
  }catch(_){}
}

// ── Patrol ──
let patrolChannels=[],patrolReversed=false,patrolLoopMode=true;
async function loadPatrolChannels(){
  const d=await fetch('/api/patrol/channels').then(r=>r.json());
  patrolChannels=d.channels||[];
  renderChannelEditor();
}
function renderChannelEditor(){
  const el=document.getElementById('ch-editor');
  if(patrolChannels.length===0){ el.innerHTML='<div class="no-devices">チャンネルなし</div>'; document.getElementById('btn-ch-save').disabled=true; return; }
  el.innerHTML=patrolChannels.map((ch,i)=>
    '<div class="ch-row">'
    +'<span class="ch-num">'+(i+1)+'.</span>'
    +'<input type="number" value="'+ch+'" min="1" max="9999" style="width:75px"'
    +' onchange="patrolChannels['+i+']=parseInt(this.value)||1;document.getElementById(\'btn-ch-save\').disabled=false">'
    +'<button class="secondary" style="padding:2px 8px;font-size:0.8em" onclick="removeChannel('+i+')">✕</button>'
    +'</div>'
  ).join('');
  document.getElementById('btn-ch-save').disabled=false;
}
function addChannel(){ const v=parseInt(prompt('追加するチャンネル番号:',''))||0; if(v>0){patrolChannels.push(v);renderChannelEditor();} }
function removeChannel(i){ patrolChannels.splice(i,1); renderChannelEditor(); document.getElementById('btn-ch-save').disabled=false; }
function sortChannels(dir){ patrolChannels.sort((a,b)=>dir==='asc'?a-b:b-a); renderChannelEditor(); document.getElementById('btn-ch-save').disabled=false; }
function bulkImportChannels(){
  const nums=document.getElementById('ch-bulk-input').value.split(/[,\s]+/).map(s=>parseInt(s)).filter(n=>n>0);
  if(!nums.length) return;
  patrolChannels=nums; renderChannelEditor(); document.getElementById('ch-bulk-input').value=''; document.getElementById('btn-ch-save').disabled=false;
}
async function saveChannels(){
  const r=await fetch('/api/patrol/channels',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({channels:patrolChannels})});
  const d=await r.json();
  const st=document.getElementById('ch-save-status');
  st.textContent=d.ok?'✓ 保存済':'✗ 失敗';
  if(d.ok){document.getElementById('btn-ch-save').disabled=true;loadPatrolChannels();}
  setTimeout(()=>st.textContent='',3000);
}
function toggleReversed(){ patrolReversed=!patrolReversed; const b=document.getElementById('btn-reversed'); b.textContent=patrolReversed?'⬇ 逆順':'⬆ 正順'; b.classList.toggle('active',patrolReversed); }
function toggleLoop(){ patrolLoopMode=!patrolLoopMode; const b=document.getElementById('btn-loop'); b.textContent=patrolLoopMode?'🔁 ループ':'1️⃣ 一巡'; b.classList.toggle('active',!patrolLoopMode); }
async function patrolStart(){
  const r=await fetch('/api/patrol/start',{method:'POST',headers:{'Content-Type':'application/json'},
    body:JSON.stringify({serials:selectedSerials(),channels:patrolChannels,reversed:patrolReversed,loop_mode:patrolLoopMode,start_channel:parseInt(document.getElementById('patrol-start-ch').value)||0})});
  const d=await r.json();
  if(!d.ok) alert('巡回開始失敗: '+(d.error||''));
}
async function patrolStop(){ await fetch('/api/patrol/stop',{method:'POST'}); }
function updatePatrolUI(running){ document.getElementById('btn-patrol-start').disabled=running; document.getElementById('btn-patrol-stop').disabled=!running; }
async function pollPatrolStatus(){
  try{
    const d=await fetch('/api/patrol/status').then(r=>r.json());
    const stateEl=document.getElementById('ps-state');
    const chEl=document.getElementById('ps-ch');
    const progEl=document.getElementById('ps-prog');
    const parEl=document.getElementById('ps-parallel');
    if(d.running){
      stateEl.className='running';
      stateEl.textContent='▶ 巡回中'+(d.waiting_move?' ⏳':'');
      chEl.textContent='Ch'+d.current_channel;
      progEl.textContent=(d.current_index+1)+'/'+d.total_channels;
      const delay=d.parallel_group_delay>0?'(+'+d.parallel_group_delay+'s)':'';
      parEl.textContent=(d.parallel_limit===0?'並列:無制限':'並列:'+d.parallel_limit+'台'+delay)
        +(d.move_timeout_secs>0?' | timeout:'+d.move_timeout_secs+'s':'')
        +' | 滞在:'+Math.round(d.dwell_secs)+'s';
      updatePatrolUI(true);
    }else{
      stateEl.className='stopped'; stateEl.textContent='■ 停止中';
      chEl.textContent=d.last_channel>0?'前回: Ch'+d.last_channel:'';
      progEl.textContent=''; parEl.textContent='';
      updatePatrolUI(false);
    }
    const fullEl=document.getElementById('ps-full');
    fullEl.textContent=(d.full_channels&&d.full_channels.length)?'🚫 満員スキップ: Ch'+d.full_channels.join(', Ch'):'';
  }catch(_){}
  setTimeout(pollPatrolStatus,2000);
}

// ── Config ──
const CFG_FIELDS=[
  {k:'discord_webhook',label:'Discord Webhook URL',type:'text',desc:'空にするとDiscord通知無効'},
  {k:'chat_exclude',label:'チャット除外キーワード',type:'csv',desc:'カンマ区切り。例: いない,終わった'},
  {k:'patrol_dwell_secs',label:'滞在時間 (秒)',type:'number',desc:'ch移動完了後〜次ch移動開始までの待機秒数'},
  {k:'patrol_move_timeout_secs',label:'移動待ちタイムアウト (秒)',type:'number',desc:'[0x2E]パケットを待つ最大秒数。0=無効'},
  {k:'parallel_limit',label:'並列切替台数',type:'number',desc:'0=全台同時（ディレイ無効）'},
  {k:'parallel_group_delay_secs',label:'グループ間ディレイ (秒)',type:'number',desc:'並列台数>0のとき有効'},
  {k:'adb_path',label:'ADBパス',type:'text',desc:'adb.exeのフルパスまたは「adb」'},
  {k:'mumu_delay_ms',label:'ADBコマンド間隔 (ms)',type:'number',desc:'各ADBコマンド間の待機時間'},
  {k:'mumu_tap_x',label:'タップX座標',type:'number',desc:'チャンネル入力欄のタップX'},
  {k:'mumu_tap_y',label:'タップY座標',type:'number',desc:'チャンネル入力欄のタップY'},
  {k:'mumu_pre_keycode',label:'プリキーコード',type:'text',desc:'チャンネル入力欄を開くキーコード'},
];
let cfgData={};
async function loadConfig(){
  cfgData=await fetch('/api/config').then(r=>r.json());
  document.getElementById('cfg-form').innerHTML=CFG_FIELDS.map(function(f){
    var val=cfgData[f.k]!==undefined?cfgData[f.k]:'';
    if(f.type==='csv'&&Array.isArray(val)) val=val.join(',');
    var inputType=f.type==='csv'?'text':f.type;
    var noteHtml=f.desc?('<span class="cfg-note">'+f.desc+'</span>'):'';
    return '<div class="cfg-field"><label>'+f.label+'</label>'
      +'<input type="'+inputType+'" id="cfg-'+f.k+'" value="'+val+'" placeholder="'+(f.desc||'')+'">'
      +noteHtml+'</div>';
  }).join('');
}
async function saveConfig(){
  const updated={...cfgData};
  CFG_FIELDS.forEach(f=>{
    const el=document.getElementById('cfg-'+f.k); if(!el) return;
    if(f.type==='number') updated[f.k]=parseFloat(el.value)||0;
    else if(f.type==='csv') updated[f.k]=el.value.split(',').map(s=>s.trim()).filter(Boolean);
    else updated[f.k]=el.value;
  });
  const r=await fetch('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(updated)});
  const d=await r.json();
  const st=document.getElementById('cfg-status');
  st.textContent=d.ok?'✓ 保存・反映済':'✗ 失敗: '+(d.error||'');
  setTimeout(()=>st.textContent='',4000);
  cfgData=updated;
}

// ── Init ──
restoreLayout();
refreshDevices();
setInterval(refreshDevices,10000);
loadPatrolChannels();
pollPatrolStatus();
loadConfig();
loadGoldHistory();
setInterval(loadGoldHistory,30000);
</script>
</body>
</html>`