#!/bin/bash
set -euo pipefail

# Container-Optimized OS host iptables drops incoming UDP by default even when VPC firewall allows 19132/udp.
/sbin/iptables -C INPUT -p udp --dport 19132 -j ACCEPT 2>/dev/null || /sbin/iptables -I INPUT 1 -p udp --dport 19132 -j ACCEPT

mkdir -p /var/minecraft/staging
chmod 777 /var/minecraft/staging

CONTAINER_NAME="minecraft-bedrock"
docker stop $CONTAINER_NAME || true
docker rm $CONTAINER_NAME || true

# itzg/minecraft-bedrock-server overwrites server.properties on every start with LEVEL_NAME (default "Bedrock level") and LEVEL_SEED.
# Read the persisted values first so container recreation preserves the active world and seed.
ACTIVE_WORLD=$(docker run --rm -v minecraft-data:/data:ro alpine sh -c '
if [ -f /data/server.properties ]; then
    grep "^level-name=" /data/server.properties | tail -n 1 | cut -d= -f2- | tr -d "\r[:space:]"
fi
' 2>/dev/null || true)

if ! printf '%s' "$ACTIVE_WORLD" | grep -Eq '^[a-zA-Z0-9_-]+$'; then
    ACTIVE_WORLD="kiseki"
fi

ACTIVE_SEED=$(docker run --rm -v minecraft-data:/data:ro alpine sh -c '
if [ -f /data/server.properties ]; then
    grep "^level-seed=" /data/server.properties | tail -n 1 | cut -d= -f2- | tr -d "\r\n"
fi
' 2>/dev/null || true)

docker run --rm -e ACTIVE_WORLD="$ACTIVE_WORLD" -v minecraft-data:/data google/cloud-sdk:alpine sh -c '
if [ ! -d "/data/worlds/$ACTIVE_WORLD" ]; then
    echo "ワールドデータ ($ACTIVE_WORLD) が見つかりません。GCS から自動リストアを試行します..."
    BUCKET="gs://$(gcloud config get-value project)-minecraft-backup"
    gcloud storage cp "$BUCKET/world-data-$ACTIVE_WORLD.tar.gz" /tmp/world.tar.gz --quiet || true
    if [ -f "/tmp/world.tar.gz" ]; then
        mkdir -p /data/worlds && cd /data/worlds && tar -xzf /tmp/world.tar.gz && rm -f /tmp/world.tar.gz
        echo "GCS からの自動リストア (.tar.gz) が完了しました。"
    else
        gcloud storage cp "$BUCKET/world-data-$ACTIVE_WORLD.tar.xz" /tmp/world.tar.xz --quiet || true
        if [ -f "/tmp/world.tar.xz" ]; then
            mkdir -p /data/worlds && cd /data/worlds && tar -xf /tmp/world.tar.xz && rm -f /tmp/world.tar.xz
            echo "GCS からの自動リストア (.tar.xz) が完了しました。"
        fi
    fi
fi
' || true

# -i keeps stdin open, which is required for docker exec send-command console injection.
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
    -e LEVEL_NAME="$ACTIVE_WORLD" \
    -e LEVEL_SEED="$ACTIVE_SEED" \
    itzg/minecraft-bedrock-server:latest
