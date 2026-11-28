#!/bin/bash
# Boot script of the monitor instance (ADR-0029), run on every boot by its user data:
#
#   monitor-boot.sh <bucket> <version> <app-stack-name>
#
# Runs Prometheus, Alertmanager, the blackbox probe, Loki and Grafana as containers on one
# Docker network, with the configuration files of the release (monitoring/, the same files
# docker compose uses locally). Images come from Docker Hub: the monitor is in a public
# subnet. Scrape targets come from EC2 (the stack's coordinator and nodes, by Name tag):
# the instance role may describe instances. Secrets (the probe key, the Telegram bot,
# Grafana's admin password) come from Secrets Manager into files, never into the template.
# Grafana, Prometheus and Alertmanager listen on 127.0.0.1 only: reached through SSM port
# forwarding (`make grafana`). Loki listens on the VPC side for Alloy on the other nodes.
set -euo pipefail   # no -x: the script handles secrets

BUCKET=$1 VERSION=$2 STACK=$3
TOKEN=$(curl -sf -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 300')
REGION=$(curl -sf -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/region)
ROOT=/opt/kvmon
secret() { aws secretsmanager get-secret-value --secret-id "$1" --query SecretString --output text --region "$REGION"; }

command -v docker >/dev/null || dnf install -y docker
systemctl enable --now docker

# 1. The release's monitoring configuration.
mkdir -p "$ROOT/secrets/telegram" "$ROOT/targets"
aws s3 cp "s3://$BUCKET/releases/$VERSION/monitoring.tar.gz" /tmp/monitoring.tar.gz --region "$REGION"
rm -rf "$ROOT/monitoring" && mkdir -p "$ROOT/monitoring"
tar -xzf /tmp/monitoring.tar.gz -C "$ROOT/monitoring"
# macOS tar adds AppleDouble files (._name) for extended attributes; Grafana tries to parse
# ._dashboards.yml as provisioning and exits (M4 lab). `make release` leaves them out now;
# this also cleans a release made before that.
find "$ROOT/monitoring" -name '._*' -delete

# 2. Secrets into files. The monitoring secret is JSON:
#    {"telegram_token": "...", "telegram_chat_id": "...", "grafana_password": "..."}
( umask 077
  secret velmakina-kv/probe-key > "$ROOT/secrets/probe.key"
  secret velmakina-kv/monitoring > "$ROOT/secrets/monitoring.json" )
python3 - "$ROOT/secrets" <<'EOF'
import json, os, sys
d = sys.argv[1]
m = json.load(open(os.path.join(d, "monitoring.json")))
for name, path in [("telegram_token", "telegram/telegram_token"), ("telegram_chat_id", "telegram/telegram_chat_id"),
                   ("grafana_password", "grafana_password")]:
    if m.get(name):
        with open(os.path.join(d, path), "w") as f:
            f.write(str(m[name]))
os.remove(os.path.join(d, "monitoring.json"))
EOF
chmod -R a+rX "$ROOT/secrets"   # the containers run as their own users; only root can log in here

# 3. Scrape targets: the stack's coordinator and three nodes, by Name tag. The instances
#    are created together with this one, so wait until all four have private IPs.
ips() {
  aws ec2 describe-instances --region "$REGION" \
    --filters "Name=tag:aws:cloudformation:stack-name,Values=$STACK" "Name=tag:Name,Values=$1" \
              "Name=instance-state-name,Values=pending,running" \
    --query 'Reservations[].Instances[].PrivateIpAddress' --output text
}
for _ in $(seq 60); do
  COORD=$(ips velmakina-kv-coordinator)
  NODES=$(ips 'velmakina-kv-node-a,velmakina-kv-node-b,velmakina-kv-node-c')   # the base nodes (shard-3's come and go)
  [ -n "$COORD" ] && [ "$(echo $NODES | wc -w)" -eq 3 ] && break
  sleep 5
done
echo "[{\"targets\": [\"$COORD:9180\"]}]" > "$ROOT/targets/coordinator.json"
# Replicas: from the coordinator's service discovery (/sd on its metrics port, ADR-0035),
# so shard-3's instances are scraped when they register. Prometheus reaches it as
# "coordinator" (--add-host below), the same name the local Compose stack uses.
echo "[{\"targets\": [\"http://$COORD:8080/v1/tables/probe/items/probe\"]}]" > "$ROOT/targets/probe.json"
echo "monitor-boot: coordinator $COORD, nodes $NODES"

# 4. The containers, on one network so they find each other by name.
docker network inspect kvmon >/dev/null 2>&1 || docker network create kvmon
for c in prometheus alertmanager blackbox loki grafana; do docker rm -f "$c" >/dev/null 2>&1 || true; done
# Prometheus' and Loki's data on the instance's disk, owned by the users the images run as
# (nobody 65534, loki 10001). The containers are recreated on every boot, and the lab stops
# and starts the instances between sessions: without these, Prometheus got a new anonymous
# volume each time and Loki kept its data inside the container, so both came back empty
# (M4 lab session 2, docs/m4/evidence/aws-deploy.txt R1). They still go with the stack.
mkdir -p "$ROOT/data/prometheus" "$ROOT/data/loki"
chown 65534:65534 "$ROOT/data/prometheus"
chown 10001:10001 "$ROOT/data/loki"
M=$ROOT/monitoring
run() { docker run -d --restart always --network kvmon --name "$@"; }
run prometheus -p 127.0.0.1:9090:9090 --add-host "coordinator:$COORD" \
  -v "$M/prometheus.yml:/etc/prometheus/prometheus.yml:ro" -v "$M/rules.yml:/etc/prometheus/rules.yml:ro" \
  -v "$ROOT/targets:/etc/prometheus/targets:ro" -v "$ROOT/data/prometheus:/prometheus" \
  prom/prometheus:v3.1.0 --config.file=/etc/prometheus/prometheus.yml --storage.tsdb.retention.time=24h
run alertmanager -p 127.0.0.1:9093:9093 --entrypoint /bin/sh \
  -v "$M/alertmanager/start.sh:/start.sh:ro" -v "$ROOT/secrets/telegram:/secrets:ro" \
  prom/alertmanager:v0.28.0 /start.sh
run blackbox -v "$M/blackbox.yml:/etc/blackbox/blackbox.yml:ro" -v "$ROOT/secrets/probe.key:/etc/blackbox/probe.key:ro" \
  prom/blackbox-exporter:v0.25.0 --config.file=/etc/blackbox/blackbox.yml
run loki -p 3100:3100 -v "$M/loki.yml:/etc/loki/loki.yml:ro" -v "$ROOT/data/loki:/loki" grafana/loki:3.3.2 -config.file=/etc/loki/loki.yml
run grafana -p 127.0.0.1:3000:3000 \
  -e GF_SECURITY_ADMIN_PASSWORD__FILE=/run/secrets/grafana_password -e GF_AUTH_ANONYMOUS_ENABLED=false \
  -e GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=/var/lib/grafana/dashboards/capstone-kv.json \
  -v "$ROOT/secrets/grafana_password:/run/secrets/grafana_password:ro" \
  -v "$M/grafana/provisioning:/etc/grafana/provisioning:ro" -v "$M/grafana/dashboards:/var/lib/grafana/dashboards:ro" \
  grafana/grafana:11.4.0
echo "monitor-boot: running $(docker ps --format '{{.Names}}' | tr '\n' ' ')"
