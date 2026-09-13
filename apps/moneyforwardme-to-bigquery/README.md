# MoneyForward ME to BigQuery Pipeline

マネーフォワードME（有料プラン）から家計簿明細データを定期的に自動エクスポートし、BigQuery へ重複なくアペンド蓄積するパイプラインコンポーネント。

---

## ディレクトリ構成

```text
apps/moneyforwardme-to-bigquery/
├── pyproject.toml                         # 依存関係定義 (FastAPI, Playwright, google-cloud-*)
├── uv.lock                                # 依存バージョン完全固定ロックファイル
├── manual_refresh_session.py              # 手元PC用: 初回MFAログイン & Secret Manager同期 (PEP 723)
├── sync.py                                # Cloud Run用: HTTPサーバー & CSV取得 & BQ MERGE & セッション自動延長
├── entrypoint.sh                          # Cloud Run用: コンテナ起動スクリプト (uv環境構築 & sync.py起動)
├── trigger-moneyforwardme-to-bigquery.sh  # GCE用: OIDCトークン取得 & Cloud Run呼び出しスクリプト
├── tests/                                 # 単体仕様テスト
│   ├── sync_test.py                       # Python仕様テスト (20件)
│   ├── manual_refresh_session_test.py     # 認証・同期仕様テスト (10件)
│   ├── trigger-moneyforwardme-to-bigquery_test.sh # トリガースクリプト仕様テスト (10件)
│   └── entrypoint_test.sh                 # コンテナ起動仕様テスト (2件)
└── README.md                              # 本ドキュメント
```

---

## コンポーネント詳細

### `manual_refresh_session.py`（手元PC用 認証同期スクリプト）
* **役割**: 人間がブラウザでマネーフォワードMEにログイン（パスワード・2段階認証など）し、生成された最新セッション Cookie（`storage_state`）を Google Cloud Secret Manager（`mf-session-cookie`）に保存します。
* **特徴**:
  * PEP 723 準拠。`uv run manual_refresh_session.py` で単体実行可能。
  * ローカルの Playwright Chromium を起動し、ログイン完了画面（`/cf` や `/home`）を自動検知して終了。

### `sync.py`（Cloud Run サービス本体）
* **役割**: GCE からの HTTP POST リクエストを受信し、マネーフォワードMEから CSV を取得して BigQuery へ重複なくアペンド蓄積します（アクセスごとのセッション自動延長機能付き）。

### `trigger-moneyforwardme-to-bigquery.sh`（GCE用トリガースクリプト）
* **役割**: GCE `always_free` 上で定期実行（systemd timer）または手動バックフィル時に呼び出されます。
* **特徴**:
  * GCE のメタデータサーバーから Cloud Run 呼び出し用の OIDC ID トークンを取得。
  * Cloud Run の URL（環境変数 `SERVICE_URL` または `gcloud` 自動解決）に対して Bearer トークン付き POST リクエストを送信。
  * 引数により「当月・先月・前々月のデフォルト同期」「単月バックフィル」「期間指定バックフィル」に対応。

---

## テストの実行方法

本モジュールの全テストは、外部（マネーフォワードMEやGoogle Cloud）と一切通信せず、ローカル環境で非破壊に実行可能です：

```bash
# Python テスト (34件) の実行
uv run --extra dev pytest -v

# Shell スクリプトテスト (12件) の実行
bash tests/trigger-moneyforwardme-to-bigquery_test.sh
bash tests/entrypoint_test.sh
```

ルートディレクトリの `Makefile` からも一括実行できます：
```bash
make test-moneyforwardme-to-bigquery
```
