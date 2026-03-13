# LoyalBoarlet Monitor

下記プロジェクトの改変版です。  
https://github.com/yukkuman/bpsr-loyal-emu

このバージョンでは巡回機能の強化、操作性改善、GUI の全面刷新が含まれています。  
複数インスタンスでスターレゾナンスでゴールドウリボを自動検知し、Discord へ通知するツール。

---

## 配布版の使い方

### 必要なソフトウェア

| ソフト | 用途 | 入手先 |
|---|---|---|
| **Npcap** | ネットワークキャプチャ | https://npcap.com/ |

> Npcap インストール時は **「WinPcap API-compatible Mode」にチェック**を入れてください。

### セットアップ

1. `config.json` の `discord_webhook` に通知先の Webhook URL を設定
2. `LoyalBoarlet.exe` を起動
3. ブラウザで `http://127.0.0.1:8080` が開く（または自動で GUI ウィンドウが起動）
4. エミュを検知させ、巡回チャンネル一覧にチャンネルを挿入
5. チャンネル入力欄の座標を設定（初期設定は解像度 1280×720 前提）

### 配布ファイル一覧

```
LoyalBoarlet.exe        ← 実行ファイル
config.json             ← 設定ファイル
channels.txt            ← 巡回チャンネルリスト（1行1番号）
data/
  locations.json        ← マップ場所名データ
```

---

## GUI について

起動後、ブラウザまたは WebView2 ウィンドウで GUI が開きます。

### パネル構成

| パネル | 内容 |
|---|---|
| 📱 デバイス一覧 & 手動切替 | ADB で認識した端末の表示・個別/一括チャンネル切替 |
| 🔁 チャンネル巡回 | 巡回の開始・停止・設定、チャンネルリストの編集 |
| 🌟 金ウリボ検知履歴 | 検知した時刻・Ch・場所の履歴（最大50件、新しい順） |
| 📋 検知ログ | リアルタイムログ表示 |
| ⚙ 設定 | config.json の各設定をリアルタイム編集・保存 |

### パネル操作

- **ヘッダーをドラッグ** → 同カラム内での並び替え、または左右カラム間への移動
- **中央の境界線をドラッグ** → 左右カラムの幅を調整
- **右上の「─」ボタン** → パネルを最小化（「＋」で展開）
- パネルの配置・幅・最小化状態は自動保存され、次回起動時に復元されます

### 巡回機能

- **チャンネルリスト編集**：行ごとに追加・削除・編集、昇順/降順ソート、カンマ区切り一括入力対応
- **巡回方向**：正順・逆順をボタンで切り替え
- **ループ / 一巡モード**：全チャンネルを一周後に自動停止するモードあり
- **開始 Ch 指定**：任意チャンネルから開始（0 = 前回位置から再開）
- **ch 移動完了待ち**：`[0x2E]` パケットを受信してからタイマーを開始（設定画面で調整可能）
- **満員 Ch の自動スキップ**：移動完了シグナルが一定時間内に届かない場合、満員と判定してスキップ
- **金ウリボ検知時の自動削除**：検知された Ch は巡回リストから自動で除去され、ファイルにも即時保存される

---

## ビルド方法（開発者向け）

### 前提条件

- Go 1.23+
- MinGW-w64 (GCC) が PATH に存在すること
- [Npcap SDK](https://npcap.com/#download) を `C:\npcap-sdk` に展開

### ビルドコマンド

```powershell
# リリースビルド（コンソール非表示）
.\build.ps1

# デバッグビルド（コンソール表示あり）
.\build.ps1 -Debug

# Npcap SDK のパスを指定する場合
.\build.ps1 -NpcapSdk "C:\path\to\npcap-sdk"
```

出力先: `release\LoyalBoarlet.exe` および `LoyalBoarlet-vX.Y.Z-windows-amd64.zip`

### 手動ビルド

```powershell
$env:CGO_ENABLED = "1"
$env:CGO_CFLAGS  = "-IC:\npcap-sdk\Include"
$env:CGO_LDFLAGS = "-LC:\npcap-sdk\Lib\x64 -lwpcap"
go build -ldflags "-s -w -H windowsgui -X main.Version=dev" -o LoyalBoarlet.exe .
```

### build.bat について

`build.bat` を実行すると `LoyalBoarlet/` フォルダにビルド済みファイルがまとめて出力されます。  
`winres/winres.json` と `winres/icon.ico` を置いておくと EXE にアイコンが自動埋め込みされます（要 `go-winres`、未インストールの場合は自動でインストールされます）。

---

## 設定ファイル (config.json)

設定は GUI の ⚙ 設定パネルから変更できます。一部の項目は保存後すぐに反映されます。

### キャプチャ設定

| キー | デフォルト | 説明 |
|---|---|---|
| `network` | `"auto"` | NIC 名。`"auto"` で最アクティブな NIC を自動選択 |
| `auto_check` | `3` | `"auto"` 時のサンプリング秒数 |
| `locations` | `"data/locations.json"` | マップ場所名データのパス |

### 通知設定

| キー | デフォルト | 説明 |
|---|---|---|
| `discord_webhook` | `""` | Discord Webhook URL。空で無効 |
| `debounce_seconds` | `30` | 同 Ch・同場所の重複通知を抑制する秒数 |
| `chat_exclude` | `[]` | ワールドチャット検知を抑制するキーワード一覧（例: `["いない","終わった"]`） |

### GUI / ADB 設定

| キー | デフォルト | 説明 |
|---|---|---|
| `gui_port` | `8080` | Web GUI のポート番号。0 で GUI 無効 |
| `adb_path` | `"adb"` | adb.exe のパスまたはファイル名 |
| `mumu_serials` | `[]` | ADB シリアル一覧。空で自動検出 |
| `mumu_tap_x` | `975` | チャンネル入力欄のタップ X 座標 |
| `mumu_tap_y` | `664` | チャンネル入力欄のタップ Y 座標 |
| `mumu_clear_length` | `3` | 入力前に DEL を送る回数 |
| `mumu_pre_keycode` | `"KEYCODE_P"` | チャンネル入力欄を開くキーコード |
| `mumu_delay_ms` | `800` | ADB コマンド間のウェイト (ms) |

### チャンネル巡回設定

| キー | デフォルト | 説明 |
|---|---|---|
| `patrol_channels_file` | `"channels.txt"` | 巡回チャンネルリストのファイルパス |
| `patrol_dwell_secs` | `60` | ch 移動完了後〜次 ch 移動開始までの待機秒数 ★即時反映 |
| `patrol_move_timeout_secs` | `30` | `[0x2E]` パケットを待つ最大秒数。0 = 無効 ★即時反映 |
| `parallel_limit` | `0` | 同時に切り替える最大台数。0 = 全台同時（グループ間ディレイも無効）★即時反映 |
| `parallel_group_delay_secs` | `0` | グループ間の待機秒数。`parallel_limit > 0` のとき有効 ★即時反映 |
| `patrol_serials` | `[]` | 巡回に使う ADB シリアル一覧。空で全デバイスを使用 |

★ = GUI の設定パネルで保存後すぐに反映（再起動不要）

---

## ライセンス

このプロジェクトは [GNU Affero General Public License v3.0 (AGPL-3.0)](LICENSE) のもとで公開されています。

### 原著作物について

本ソフトウェアは [balrogsxt/StarResonanceAPI](https://github.com/balrogsxt/StarResonanceAPI)（Copyright (C) balrogsxt）を元に改変・拡張したものです。

AGPL-3.0 に基づき、以下の条件が適用されます：

- 改変版を配布・公開する場合は、改変後のソースコードも AGPL-3.0 で公開する必要があります
- 元の著作権表示を維持する必要があります
- 派生物も同じく AGPL-3.0 ライセンスを適用する必要があります
