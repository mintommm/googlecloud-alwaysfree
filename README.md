```markdown
---
project: discord-integrated-minecraft-server
architecture: gcp-terraform-github-actions-golang
ssot: github-repository
credentials_management: gcp-secret-manager
last_updated: 2026-07-12
---

# Discord連携マイクラサーバーGCE構築プロジェクト

## 概要
本プロジェクトは、Google Cloud Platform（GCP）上に構築されたマインクラフト（Bedrock版）ゲームサーバー、およびそれを制御するDiscord Bot環境を、Terraformによるインフラのコード化（IaC）とGitHub ActionsによるCI/CDパイプラインを用いて自動管理するマルチモジュール（モノレポ）リポジトリである。

## 単一の正（SSOT）の定義と機密情報管理
本環境におけるすべての「あるべき姿」は、本GitHubリポジトリにコミットされたコードを正（Single Source of Truth）とする。
- リモートリポジトリへの秘密情報の流出を完全に防ぐため、ローカル環境での認証情報の平文保存（`terraform.tfvars` 等への記述）は禁止とする。
- 認証情報はGCPの「Secret Manager」を利用して一元管理し、Always Free（無料枠）の範囲内で安全に参照を行う。

## コードから非自明な暗黙の制約（AIエージェントへの指示）

### 1. 変数 `rcon_password` と GCP シークレットのマッピング
Terraform定義（`variables.tf`）における `rcon_password` に注入すべき本番パスワードは、GCP Secret Manager上のシークレット名 **`minecraft-rcon-password`** に格納されている。ローカル検証および手動適用時は必ずここから動的に取得して注入すること。

### 2. `minecraft01` インスタンスの「実質的な破壊厳禁」制約
`google_compute_instance.minecraft01` は、HCLコード上 `prevent_destroy` は未設定であるが、運用上の要請（動的外部IPの維持およびコンテナ環境の安定）により、**リプレイス（破壊・再生成）を伴う変更の適用は原則禁止**とする。
メタデータ変更等は必ず `metadata` ブロックのマップ構造（`startup-script` キー）を使用し、`update in-place` で処理させること。差分判定のバグ等でリプレイスが計画された場合は、Stateの再インポート（`terraform state rm` / `import`）によりインプレース構造へ強制適合させること。

---

## 開発・運用手順（Cloud Shell等での実行）

### 1. シークレットの初回登録・更新
```bash
# シークレットの作成（未作成の場合のみ）
gcloud secrets create minecraft-rcon-password --replication-policy="automatic"

# パスワード値の登録・変更（バージョン追加）
echo -n "実際のRCONパスワード文字列" | gcloud secrets versions add minecraft-rcon-password --data-file=-

```

### 2. ローカル検証および適用手順

ファイルにパスワードを永続化させず、コマンド実行時のみ環境変数を動的に注入して整合性を検証する。必ず `infrastructure/` ディレクトリに移動した上で実行すること。

```bash
# 実行計画の確認（Plan）
TF_VAR_rcon_password=$(gcloud secrets versions access latest --secret="minecraft-rcon-password") terraform plan

# 実行計画の適用（Apply）
TF_VAR_rcon_password=$(gcloud secrets versions access latest --secret="minecraft-rcon-password") terraform apply -auto-approve

```
