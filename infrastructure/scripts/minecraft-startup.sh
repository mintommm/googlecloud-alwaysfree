#!/bin/bash
set -euo pipefail

# 1. 内部ファイアウォール（iptables）の通信許可（先頭挿入 -I により順序エラーを恒久対策）
/sbin/iptables -C INPUT -p udp --dport 19132 -j ACCEPT 2>/dev/null || /sbin/iptables -I INPUT 1 -p udp --dport 19132 -j ACCEPT
/sbin/iptables -C INPUT -p tcp --dport 25575 -j ACCEPT 2>/dev/null || /sbin/iptables -I INPUT 1 -p tcp --dport 25575 -j ACCEPT

# 2. 同名コンテナが既に存在する場合はクリーンアップ
CONTAINER_NAME="minecraft-bedrock"
docker stop $CONTAINER_NAME || true
docker rm $CONTAINER_NAME || true

# 3. 名前付きボリュームを使用したコンテナの起動（LEVEL_NAME="Kiseki" および Allowlist を明示）
docker run -d \
    --name=$CONTAINER_NAME \
    --restart=always \
    -p 19132:19132/udp \
    -p 25575:25575/tcp \
    -v minecraft-data:/data \
    -e EULA=TRUE \
    -e RCON_ENABLED=true \
    -e RCON_PASSWORD=${rcon_password} \
    -e ALLOW_LIST=true \
    -e ALLOW_LIST_USERS="MockPencil3834,DaftBurrito7340,superkurute,StaticEar839559" \
    -e LEVEL_NAME="Kiseki" \
    itzg/minecraft-bedrock-server:latest
