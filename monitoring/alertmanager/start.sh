#!/bin/sh
# Starts Alertmanager with alerts to Telegram (ADR-0029) when the bot's token and chat ID
# are mounted in /secrets, and with no receiver otherwise (alerts then show only in the
# Alertmanager and Grafana UIs). The token stays in its file (bot_token_file); only the
# chat ID, a number, is written into the config. Timings as in the design: group_wait 5 s,
# group_interval 30 s, repeat_interval 1 h (the Watchdog's hourly message).
set -eu
CONFIG=/tmp/alertmanager.yml
if [ -s /secrets/telegram_token ] && [ -s /secrets/telegram_chat_id ]; then
  CHAT_ID=$(tr -dc '0-9-' < /secrets/telegram_chat_id)
  RECEIVER=telegram
  RECEIVERS="  - name: telegram
    telegram_configs:
      - bot_token_file: /secrets/telegram_token
        chat_id: $CHAT_ID
        parse_mode: HTML
        send_resolved: true"
else
  echo "no Telegram secrets in /secrets: alerts go nowhere (UI only)" >&2
  RECEIVER=none
  RECEIVERS="  - name: none"
fi
cat > "$CONFIG" <<EOF
route:
  receiver: $RECEIVER
  group_by: [alertname, node, shard]
  group_wait: 5s
  group_interval: 30s
  repeat_interval: 1h
receivers:
$RECEIVERS
EOF
exec /bin/alertmanager --config.file="$CONFIG" --storage.path=/alertmanager "$@"
