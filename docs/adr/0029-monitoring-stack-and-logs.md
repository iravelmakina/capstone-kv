# ADR-0029: Monitoring stack, logs and alert delivery

- Status: **Accepted** — 2026-11-27 by Iryna Velmakina (author) in chat ("я все приймаю"),
  together with the AI-review verdicts. Drafted with Claude Code from the decision list she
  approved; revised after the AI design review (`docs/m4/ai-review.md`).
- Date: 2026-11-27

## Context

Metrics are exposed on a metrics port (ADR-0025); something has to collect them, keep them,
draw the dashboard, evaluate alert rules and deliver alerts. Logs from the coordinator and
six replicas on four instances need to be searchable by request ID. Sandbox limits
(`docs/m1/labrole-dissection.md`): CloudWatch custom metrics, dashboards and X-Ray are
denied; our instances can't send logs to CloudWatch Logs, publish to SNS or write to S3;
they can read Secrets Manager. Private subnets have no internet route; public subnets do.
Free Datadog and Sentry are offered through the student pack. In M5 the p99 that drives
autoscaling must come from here.

## Options

| | 1. Prometheus + Grafana + Alertmanager + Loki on one EC2 instance | 2. Datadog (student plan) | 3. CloudWatch |
| --- | --- | --- | --- |
| Works in the sandbox | yes | agents on private nodes need internet: a NAT (≈ $0.05/h + data) or a proxy; the AWS integration needs an IAM role we can't create | metrics and logs from instances denied; only EC2's own metrics and alarms |
| Cost | one t3.small while on (≈ $0.02/h) | NAT while on | — |
| Dashboards and rules as files in the repo | yes (provisioning) | partly (API/Terraform) | — |
| Explainable "without notes" | yes, all configs are ours | the agent is a black box | — |
| Traces | no | yes (APM) | X-Ray denied |
| Alert delivery | Alertmanager → Telegram, Slack, email | built in | SNS denied to instances |

**Alert channel:** Telegram (a bot, free, on a phone, supported by Alertmanager without
extra components) vs Slack (needs a workspace and an app) vs email (needs an SMTP account).

**Log shipping:** Grafana Alloy on each instance reading Docker's JSON logs vs Promtail
(end of life since 2026-03-02, per Grafana's Loki documentation) vs a Loki logging-driver
plugin (a plugin download on private nodes) vs the app pushing logs itself (couples the app
to the log store).

**Access to Grafana:** public port limited to one IP vs **SSM port forwarding** (no public
ingress at all; the same path as Prometheus and Alertmanager).

## Decision

**Option 1.**

- **`monitor` instance:** t3.small in public subnet A (same AZ as the coordinator), created
  only when the app stack's `Monitoring` parameter is `true`, like the benchmark client.
  Docker Compose runs Prometheus (scrape and rule evaluation every 5 s, 24 h retention),
  Alertmanager, Grafana, Loki and the blackbox exporter. Images come from Docker Hub
  through the public subnet's internet route.
- **No public ingress.** Grafana, Prometheus and Alertmanager are reached from the laptop
  through SSM port forwarding. Loki (3100) admits the coordinator's and the shards'
  groups.
- **The monitor reaches only metrics ports** (coordinator 9180, replicas 9181/9182) and
  the coordinator's API for the probe; the replicas' API ports, which serve the log with
  every value, stay open only between the coordinator and the replicas.
- **Probe:** the blackbox exporter makes an authenticated read through the coordinator
  every 5 s, with a read-only `probe` key from Secrets Manager. Its result counts in the availability SLI
  (ADR-0025) and fires `CoordinatorDown` when the coordinator's own metrics vanish.
- **Grafana Alloy** on every instance; its image reaches the private nodes through the
  artifacts bucket, as the app image does (ADR-0009).
- **Logs:** JSON lines from `log/slog` with a request ID and the key's name; the leader
  logs the LSN it assigned, followers the LSN ranges they append. Searched in Grafana's
  Explore view.
- **Alerts:** rules in `monitoring/rules.yml`, tested with `promtool test rules`; configs
  checked with `promtool check config` and `amtool check-config`. Alertmanager groups by
  alert and node (`group_wait` 5 s, `group_interval` 30 s, `repeat_interval` 1 h) and
  delivers to a Telegram chat. An always-firing `Watchdog` alert reaches the chat every
  hour: an hour of silence means the monitor is down. The bot token, the chat ID and
  Grafana's admin password live in a Secrets Manager secret (`velmakina-kv/monitoring`),
  read by the monitor at boot, never in the repo.
- **Everything is a file:** Prometheus config, rules and their tests, the Alertmanager
  config template, Grafana data sources, the dashboard JSON, Loki, blackbox and Alloy
  configs live in `monitoring/` and are used unchanged by the local Docker Compose stack.
- **M5:** the monitor's Prometheus becomes the autoscaler's input, so the monitor stops
  being optional there. Until then, before every teardown, the evidence (query results,
  dashboard screenshots) is exported from the laptop through SSM port forwarding.

**Runner-up:** Datadog with a NAT switched on during sessions. Reconsider if the monitor
instance proves unreliable, if traces become required, or if the course requires a hosted
service.

## Accepted downsides

- The monitor is one instance: when it is down, nobody watches; the missing watchdog
  message is the only sign, up to an hour later.
- Metrics and logs are lost with the stack (`make down-app`), except what is exported.
- No spans: a request is followed by ID and LSN through log lines, without per-hop timings.
- Grafana needs an SSM session open on the laptop to be seen.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| Docker Hub rate limits on image pulls | Low | Five images, pulled once per session |
| The monitor is compromised (it runs third-party images) | Medium | No public ingress; it reaches only metrics ports and the coordinator's API, with a key that can only read |
| Loki or Prometheus fill the disk | Low | 24 h retention; the instance lives a few hours |

## Related

- ADR-0025 (metrics), ADR-0026 (SLOs behind the alert thresholds), ADR-0027 (keys),
  ADR-0005 (no internet in private subnets), ADR-0009 (delivery through S3).
