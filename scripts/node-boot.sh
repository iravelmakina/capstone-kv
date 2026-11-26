#!/bin/bash
# Boot script for every capstone-kv instance (ADR-0009, ADR-0014, ADR-0020). The app
# stack's user data downloads it from the release folder in S3 and runs it as root on
# every boot:
#
#   node-boot.sh <bucket> <version> coordinator <expected-shards> <w>
#   node-boot.sh <bucket> <version> node <coordinator-private-ip> <w> <shard>=<replica>:<port>...
#
# e.g. "node 10.20.0.10 2 shard-1=1a:8081 shard-2=2a:8082" runs two replica containers.
# It installs Docker, loads the image of <version> from S3 (through the S3 gateway
# endpoint, so private subnets need no internet) and runs each role as a container.
# Running it again is safe: an installed Docker or an already loaded image is reused.
set -euxo pipefail

BUCKET=$1 VERSION=$2 ROLE=$3
IMAGE=capstone-kv:$VERSION

# Instance metadata (IMDSv2: get a session token first, then ask with it).
TOKEN=$(curl -sf -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 300')
md() { curl -sf -H "X-aws-ec2-metadata-token: $TOKEN" "http://169.254.169.254/latest/meta-data/$1"; }
MY_IP=$(md local-ipv4)
REGION=$(md placement/region)

# 1. Docker, from the Amazon Linux package repositories (hosted in S3).
command -v docker >/dev/null || dnf install -y docker
systemctl enable --now docker

# 2. Data directories (one per container) live on the instance's EBS volume, so they
#    survive container and instance restarts; owned by the container's non-root user
#    (distroless "nonroot" = 65532).
datadir() { mkdir -p "/var/lib/kv/$1"; chown 65532:65532 "/var/lib/kv/$1"; echo "/var/lib/kv/$1"; }

# 3. The image of this version, downloaded only if it isn't loaded yet.
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  aws s3 cp "s3://$BUCKET/releases/$VERSION/kvnode.tar.gz" /tmp/kvnode.tar.gz --region "$REGION"
  docker load -i /tmp/kvnode.tar.gz
  rm -f /tmp/kvnode.tar.gz
fi

# 4. (Re)start the containers. --restart always: Docker brings them back after a crash
#    or reboot. Every kv-* container (and M2's single "kvnode") is replaced.
for c in $(docker ps -aq --filter name='^kv-' --filter name='^kvnode$'); do docker rm -f "$c" >/dev/null; done
case "$ROLE" in
  coordinator)
    EXPECTED=$4 W=$5
    docker run -d --name kv-coordinator --restart always -p 8080:8080 -v "$(datadir coordinator)":/data \
      -e KV_ROLE=coordinator -e KV_EXPECTED_SHARDS="$EXPECTED" -e KV_REPLICAS=3 -e KV_W="$W" \
      -e KV_LEADERS=shard-1=1a,shard-2=2b "$IMAGE"
    ;;
  node)
    COORDINATOR_IP=$4 W=$5
    shift 5
    for spec in "$@"; do                      # shard-1=1a:8081
      SHARD=${spec%%=*} rest=${spec#*=}
      REPLICA=${rest%%:*} PORT=${rest#*:}
      docker run -d --name "kv-$REPLICA" --restart always -p "$PORT:8080" -v "$(datadir "$REPLICA")":/data \
        -e KV_ROLE=shard -e KV_SHARD="$SHARD" -e KV_REPLICA="$REPLICA" -e KV_W="$W" \
        -e KV_COORDINATOR_URL="http://$COORDINATOR_IP:8080" \
        -e KV_ADVERTISE_URL="http://$MY_IP:$PORT" "$IMAGE"
    done
    ;;
  *)
    echo "unknown role $ROLE" >&2
    exit 1
    ;;
esac

# Old images pile up with every deploy; keep only the running one.
docker image prune -af >/dev/null || true
echo "node-boot: $ROLE running $IMAGE: $(docker ps --format '{{.Names}}' | tr '\n' ' ')"
