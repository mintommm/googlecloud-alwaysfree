#!/bin/bash
set -euo pipefail

# 1. 内部ファイアウォール（iptables）の通信許可（19132/udp のみ許可、RCON用25575は完全廃止）
/sbin/iptables -C INPUT -p udp --dport 19132 -j ACCEPT 2>/dev/null || /sbin/iptables -I INPUT 1 -p udp --dport 19132 -j ACCEPT

# 2. ホストOS上にGit永続ステージング領域を確保
mkdir -p /var/minecraft/git-repo
chmod 777 /var/minecraft/git-repo

# 3. 同名コンテナが既に存在する場合はクリーンアップ
CONTAINER_NAME="minecraft-bedrock"
docker stop $CONTAINER_NAME || true
docker rm $CONTAINER_NAME || true

# 4. 名前付きボリューム（minecraft-data）を使用したコンテナの起動
# 標準入力インジェクションを有効化するため「-i」フラグを明示的に付与
docker run -d -i \
    --name=$CONTAINER_NAME \
    --restart=always \
    -p 19132:19132/udp \
    -v minecraft-data:/data \
    -e EULA=TRUE \
    -e ALLOW_LIST=true \
    -e ALLOW_LIST_USERS="${allow_list_users}" \
    -e LEVEL_NAME="Kiseki" \
    itzg/minecraft-bedrock-server:latest
