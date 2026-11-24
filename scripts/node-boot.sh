#!/bin/bash
# Boot script for every capstone-kv instance (ADR-0009, ADR-0014). The app stack's user
# data downloads it from the release folder in S3 and runs it as root on every boot:
#
#   node-boot.sh <bucket> <version> coordinator <expected-shards>
#   node-boot.sh <bucket> <version> shard <shard-id> <coordinator-private-ip>
#
# It installs Docker, loads the image of <version> from S3 (through the S3 gateway
# endpoint, so private subnets need no internet) and runs the node as a container.
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

# 2. Data directory, owned by the container's non-root user (distroless "nonroot" = 65532).
#    It lives on the instance's EBS volume, so it survives container and instance restarts.
mkdir -p /var/lib/kv
chown 65532:65532 /var/lib/kv

# 3. The image of this version, downloaded only if it isn't loaded yet.
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  aws s3 cp "s3://$BUCKET/releases/$VERSION/kvnode.tar.gz" /tmp/kvnode.tar.gz --region "$REGION"
  docker load -i /tmp/kvnode.tar.gz
  rm -f /tmp/kvnode.tar.gz
fi

# 4. (Re)start the node. --restart always: Docker brings it back after a crash or reboot.
docker rm -f kvnode >/dev/null 2>&1 || true
case "$ROLE" in
  coordinator)
    EXPECTED=$4
    docker run -d --name kvnode --restart always -p 8080:8080 -v /var/lib/kv:/data \
      -e KV_ROLE=coordinator -e KV_EXPECTED_SHARDS="$EXPECTED" "$IMAGE"
    ;;
  shard)
    SHARD_ID=$4 COORDINATOR_IP=$5
    docker run -d --name kvnode --restart always -p 8080:8080 -v /var/lib/kv:/data \
      -e KV_ROLE=shard -e KV_SHARD_ID="$SHARD_ID" \
      -e KV_COORDINATOR_URL="http://$COORDINATOR_IP:8080" \
      -e KV_ADVERTISE_URL="http://$MY_IP:8080" "$IMAGE"
    ;;
  *)
    echo "unknown role $ROLE" >&2
    exit 1
    ;;
esac

# Old images pile up with every deploy; keep only the running one.
docker image prune -af >/dev/null || true
echo "node-boot: $ROLE running $IMAGE"
