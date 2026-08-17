#!/bin/bash
set -euo pipefail

# 1. 内部ファイアウォール（iptables）の通信許可（19132/udp のみ許可）
/sbin/iptables -C INPUT -p udp --dport 19132 -j ACCEPT 2>/dev/null || /sbin/iptables -I INPUT 1 -p udp --dport 19132 -j ACCEPT

# 2. ホストOS上にステージング領域を確保
mkdir -p /var/minecraft/staging
chmod 777 /var/minecraft/staging

# 3. 同名コンテナが既に存在する場合はクリーンアップ
CONTAINER_NAME="minecraft-bedrock"
docker stop $CONTAINER_NAME || true
docker rm $CONTAINER_NAME || true

# 4. ボリューム消失時の自動ディザスタリカバリ (GCS から最新バックアップを取得)
# ボリューム内に worlds/kiseki が存在しない場合のみ GCS からダウンロード
docker run --rm -v minecraft-data:/data google/cloud-sdk:alpine sh -c '
if [ ! -d "/data/worlds/kiseki" ]; then
    echo "ワールドデータが見つかりません。GCS から自動リストアを試行します..."
    gcloud storage cp gs://$(gcloud config get-value project)-minecraft-backup/world-data-kiseki.tar.xz /tmp/world.tar.xz --quiet || true
    if [ -f "/tmp/world.tar.xz" ]; then
        mkdir -p /data/worlds
        cd /data/worlds
        tar -xf /tmp/world.tar.xz
        rm -f /tmp/world.tar.xz
        echo "GCS からの自動リストアが完了しました。"
    fi
fi
' || true

# 5. 名前付きボリューム（minecraft-data）を使用したコンテナの起動
# 標準入力インジェクションを有効化するため「-i」フラグを付与
# ログ容量制限 (max-size=30m, max-file=3) を付与
docker run -d -i \
    --name=$CONTAINER_NAME \
    --restart=always \
    --log-opt max-size=30m \
    --log-opt max-file=3 \
    -p 19132:19132/udp \
    -v minecraft-data:/data \
    -e EULA=TRUE \
    -e ALLOW_LIST=true \
    -e ALLOW_LIST_USERS="${allow_list_users}" \
    -e LEVEL_NAME="kiseki" \
    itzg/minecraft-bedrock-server:latest
