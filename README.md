# Katala Remote

小規模なマシン群（自宅・ラボ・開発機の混在フリート）を運用するための Go 製ツール 3 本。
単一バイナリ・ランタイム依存なし・**Windows / macOS / Linux** 単一コードベース。

| コマンド | 役割 |
|---|---|
| `procd` | 各ノードに常駐。プロセス上位N・CPU・メモリ・GPU占有・センサーを JSON で返す。**read-only**（状態を変えるエンドポイントは無い） |
| `console` | hub に常駐。全ノードの `procd` を **ssh 経由**で集約し、温度は稼働中の [beszel](https://github.com/henrygd/beszel) hub の SQLite から読んで HTML と JSON API で出す |
| `rbench` | リモートデスクトップのエンコード/デコード能力を、**実際にエンコードして**実測する単発診断ツール |

3 本は独立している。`rbench` は他の 2 本と無関係に単体で使える。

## ビルド

```bash
go build ./...
go build -o bin/procd  ./cmd/procd
go build -o bin/console ./cmd/console
go build -o bin/rbench ./cmd/rbench

# クロスコンパイル
GOOS=windows GOARCH=amd64 go build -o bin/rbench.exe            ./cmd/rbench
GOOS=darwin  GOARCH=arm64 go build -o bin/rbench-darwin-arm64   ./cmd/rbench
GOOS=linux   GOARCH=amd64 go build -o bin/rbench-linux-amd64    ./cmd/rbench
```

---

## procd — ノード常駐のプロセス/センサー API

```bash
procd -addr 127.0.0.1:45877 -top 12 -interval 4s
```

| エンドポイント | 内容 |
|---|---|
| `GET /procs` | ホスト名・OS・CPU%・メモリ・load1・uptime・温度・上位N プロセス（PID/名前/ユーザー/CPU%/RSS/GPU占有） |
| `GET /health` | `{"ok": <直近サンプルが60秒以内>, "sample_age_s": ...}` |

設計上の約束:

- **read-only**。kill も設定変更も無い。書き込み系エンドポイントは実装しない前提で作ってある
- **認証機構を持たない**。既定で loopback にしかバインドしない。`-addr` を外部に晒すと無認証で公開されるので、自分のネットワーク境界の内側でだけやること
- **プロセスCPU% は差分から出す**。gopsutil の値はプロセス起動以降の平均なので、PID ごとに前回のCPU時間を保持して実経過時間で割り、コア数で正規化する。初回サンプルは行を返さない（数字を捏造しないため）

### 既知の制約（実測）

- **Windows のプロセス別VRAMは取得できない**。`nvidia-smi --query-compute-apps` は WDDM 下で `[N/A]` を返す（OSがVRAMを管理し、GeForce は TCC に切替できない）。PID一覧は正確なので `on_gpu` は真偽値として扱い、`gpu_mb=0` を「GPU未使用」と読まないこと
- **Windows の CPU温度は非管理者では読めない**。`procd` が得るのは ACPI ThermalZone と GPU 程度。CPU/マザボの温度が要るなら beszel agent を `LHM=true` + 管理者で動かす（procd はそれを二重実装しない）

## console — フリート1枚ダッシュボード

```bash
console -roster ./fleet-hosts.tsv \
        -beszel-db /path/to/beszel_data/data.db \
        -addr 127.0.0.1:8770
```

`http://127.0.0.1:8770/` がダッシュボード、`/api/fleet` が同じ内容の JSON。

| フラグ | 既定 | 意味 |
|---|---|---|
| `-roster` | `$KATALA_ROSTER` | **必須**。ノード一覧 TSV（下記） |
| `-beszel-db` | `$KATALA_BESZEL_DB` | 任意。beszel hub の SQLite を read-only で開く。空なら procd のセンサーのみ |
| `-procd-port` | `45877` | 各ノードの procd ポート |
| `-refresh` | `30s` | ポーリング間隔 |
| `-temp-warn` | `84` | この温度以上で警告を出す |

自ノード（hub 自身）は ssh を経由せず直接 HTTP で読む。環境変数 `KATALA_SELF_NODE`
に自分の `node_id` を入れておく。

### roster TSV のカラム仕様

タブ区切り、**1行目はヘッダとして読み飛ばす**。7列以上ある行だけを読み、以下の列だけを使う
（8列目以降は無視されるので、既存の台帳をそのまま食わせてよい）:

| 列 | 名前 | 用途 |
|---|---|---|
| 1 | `alias` | ssh の接続先名。`~/.ssh/config` の Host に一致させる。表示名も兼ねる |
| 2 | (任意) | 未使用 |
| 3 | `ipv4` | 表示のみ |
| 4 | `os` | 表示のみ |
| 5 | `status` | `online` **以外の行は除外**（retired 等） |
| 6 | `role` | 表示 + `no-ssh-expected` の行は除外（スマホ等、agent が居ないもの） |
| 7 | `node_id` | beszel 側のシステム名と突き合わせるキー。`KATALA_SELF_NODE` とも比較する |

```tsv
alias	dns	ipv4	os	status	role	node_id
mac-a	mac-a.example	10.0.0.4	macOS	online	primary-mac	mac-a
win-gpu-a	win-gpu-a.example	10.0.0.1	windows	online	gpu-dev	win-gpu-a
phone-a	phone-a.example	10.0.0.3	iOS	online	no-ssh-expected	phone-a
```

### なぜ収集経路に ssh を選んだのか

`procd` は全ノードで **loopback にしかバインドしない**。console は
`ssh <alias> curl localhost:45877/procs` で読む。

- プライベートネットワークの全ピア（スマホ含む）に新しい HTTP ポートを晒さずに済む。認証機構を新規に発明しない — 境界は既にある ssh 鍵
- 一部のノードは受信ファイアウォールで inbound が落ちるため、ssh 経由でしか届かない

代償: ノードあたり ssh 1回/ポーリング。既定間隔は 30 秒。

### なぜ温度を自前で集めないのか

温度・履歴・アラート・軽量エージェントは beszel が既に解いている（agent が
LibreHardwareMonitor を同梱し、Apple Silicon のセンサーも sudo 無しで読む）。
作り直せば劣化版の二重実装になる。beszel に無いのは「**どのプロセスが**食っているか」
だけなので、console はそこだけを埋め、温度は beszel の SQLite を read-only で読む。

**古いサンプルは温度として表示しない。** beszel の最新サンプルが 5 分より古ければ
procd 側のセンサーにフォールバックし、それも無ければ「不明」と出す。
取得できないノードを健全として数えない（`no procd` は緑にならない）のが
このダッシュボードの唯一の強い主張。

---

## rbench — リモートデスクトップ能力の実測

```bash
rbench            # 人間向け
rbench -json      # JSON
rbench -skip-decode
```

`ffmpeg` が PATH に必要。無い場合は 0 を返さず「NOT MEASURED」と明示する。

### なぜ一覧ではなく実測なのか

`ffmpeg -encoders` は**能力を保証しない**。実測で見つかった例:

- an Ampere-generation GeForce に `av1_nvenc` が一覧表示されるが、Ampere は AV1 エンコード非対応で `No capable devices found`
- a Blackwell-generation GeForce に `h264_nvenc` が表示されるが、当時のドライバでは `nvenc API 13.1` 要求を満たさず一切開けない
- 同じノードは **Intel QSV 経由なら AV1 303fps** で動いた。NVIDIA だけ見ていたら「エンコード不可」と誤判定していた

したがって候補コーデックを**全て実際にエンコードして**判定する。NVENC / AMF / QSV /
VAAPI / VideoToolbox を OS ごとに網羅し、ソフトウェア（libx264 ultrafast+zerolatency）を
基準に置く。デコード側も同様に、ソフトウェアエンコーダでサンプルを生成してから
各 hwaccel を実際に通す（`-hwaccel_output_format` を必ず付ける。付けないと
ハードウェアデコード失敗でも ffmpeg が exit 0 を返し「対応」と誤読する）。

### 測定の読み方（重要）

`encode_fps` は**リアルタイム余裕度**の指標にすぎない。合成パターンではソフトウェア
エンコードがハードウェアより高い fps を出すことがあるが、リモートデスクトップでは:

- ソフトはセッションが共有する CPU を消費する（ゲーム・学習と競合）
- 同品質での必要ビットレートが大きい

ため `best_codec` は**ハードウェアを優先**する。測定条件は `testsrc2 1920x1080` を
300 フレーム（セッション初期化コストを薄めるため。60 フレームでは NVENC の初期化が
支配的になる）。フラットな色のソースだと libx264 が NVENC を追い抜いて順位が反転するので、
動きとディテールのある `testsrc2` を使っている。

| フラグ | 既定 |
|---|---|
| `-frames` | `300` |
| `-width` / `-height` | `1920` / `1080` |
| `-timeout` | `25s`（コーデックごと） |
| `-skip-decode` | `false`（デコード検査は小さな一時ファイルを書く） |

### 未測定（明示）

- 受信側のデコード能力（クライアント機の実測は別途必要）
- 実セッションの end-to-end 遅延（表示側のキャプチャが必要）
- ディスクリートGPUで描画 → iGPU(QSV) でエンコードする際の転送コスト

---

## テスト

```bash
go test ./...
```

`procd` は CPU差分（初回サンプルで数字を捏造しない、コア数正規化）、`console` は
roster 除外規則、`rbench` はコーデック順位・OS別候補・失敗理由の伝播を検査する。
ffmpeg が無い環境では rbench の実エンコードテストは skip する。

## 常駐

Task Scheduler / launchd への登録は管理者権限が要ることがあるので、ユーザー領域で足りる:

- Windows: スタートアップフォルダ + `wscript` の `SW_HIDE` で起動する vbs。**vbs 内の
  再起動ループが launchd KeepAlive 相当**として働く。これが無いと落ちた時に次のログオンまで復帰しない
- macOS: `~/Library/LaunchAgents/*.plist`（`KeepAlive`）

kill → 12 秒で復帰することを複数ノードで確認済み。

## ライセンス

MIT（[LICENSE](LICENSE)）。

依存はすべて permissive で、第三者コードをこのリポジトリに同梱していない（`go.mod` 参照のみ）:

- `github.com/shirou/gopsutil/v4` — BSD-3-Clause
- `modernc.org/sqlite`, `modernc.org/libc`, `modernc.org/mathutil`, `modernc.org/memory` — BSD-3-Clause
- `golang.org/x/sys`, `github.com/google/uuid`, `github.com/lufia/plan9stats`,
  `github.com/tklauser/go-sysconf`, `github.com/remyoudompheng/bigfft` — BSD-3-Clause
- `github.com/dustin/go-humanize`, `github.com/go-ole/go-ole`, `github.com/mattn/go-isatty`,
  `github.com/ncruces/go-strftime`, `github.com/power-devops/perfstat`,
  `github.com/yusufpapurcu/wmi` — MIT
- `github.com/ebitengine/purego`, `github.com/tklauser/numcpus` — Apache-2.0（上流に NOTICE ファイルは無い）
