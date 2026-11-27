# capstone-kv: stack lifecycle (M1 infra-lifecycle spec) and, from M2, releases and the app.
# Every update goes through a change set that is printed before anything changes.
# Written for GNU Make 3.81 (macOS default): no .ONESHELL, so multi-line recipes are
# joined with "; \" and run in one shell.

SHELL    := /bin/bash
PROFILE  ?= academy
REGION   ?= us-east-1
STACK    ?= velmakina-kv-network
TEMPLATE ?= infra/network.yaml
PARAMS   ?=
# Tools from the repo's virtualenv if present (.venv/, git-ignored), else from PATH.
# The AWS CLI runs as "python -m awscli": the repo path contains a space, which breaks the
# first line (#!) of the venv's own `aws` script.
AWSCLI   := $(if $(wildcard .venv/bin/aws),.venv/bin/python -m awscli,aws)
# PROFILE= (empty) uses credentials from the environment, as in CI (repository secrets).
AWS      := $(AWSCLI) $(if $(PROFILE),--profile $(PROFILE),) --region $(REGION)
CFN      := $(AWS) cloudformation

ARTIFACTS_STACK := velmakina-kv-artifacts
APP_STACK       := velmakina-kv-app
# A release is named by its commit.
VERSION  ?= $(shell git rev-parse --short=12 HEAD)
CFNLINT  := $(if $(wildcard .venv/bin/cfn-lint),.venv/bin/cfn-lint,cfn-lint)

.PHONY: help whoami lint diff up down drift verify rebuild test keys local-up local-down rebuild-replica smoke \
        up-artifacts release up-app down-app app-url smoke-aws smoke-replica bench-aws secrets rotate-keys

help:            ## list targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*##/ —/'

whoami:          ## which AWS identity is active
	@$(AWS) sts get-caller-identity --query Arn --output text

lint:            ## static checks on all templates (no AWS calls)
	$(CFNLINT) infra/*.yaml

# diff: create a change set WITHOUT executing it and print what it would do.
# CREATE if the stack doesn't exist (or only ever had an unexecuted change set), else UPDATE.
# STACK, TEMPLATE and PARAMS (ParameterKey=..,ParameterValue=.. pairs) pick what to deploy.
diff:            ## show what a deploy would change
	@set -euo pipefail; \
	status=$$($(CFN) describe-stacks --stack-name $(STACK) --query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo NONE); \
	if [ "$$status" = NONE ] || [ "$$status" = REVIEW_IN_PROGRESS ]; then type=CREATE; else type=UPDATE; fi; \
	cs=$(STACK)-$$(date +%Y%m%d-%H%M%S); \
	$(CFN) create-change-set --stack-name $(STACK) --change-set-name $$cs --change-set-type $$type \
	  --template-body file://$(TEMPLATE) $(if $(strip $(PARAMS)),--parameters $(PARAMS),) \
	  --tags Key=Project,Value=capstone-kv >/dev/null; \
	if ! $(CFN) wait change-set-create-complete --stack-name $(STACK) --change-set-name $$cs 2>/dev/null; then \
	  reason=$$($(CFN) describe-change-set --stack-name $(STACK) --change-set-name $$cs --query StatusReason --output text); \
	  echo "$$reason"; \
	  $(CFN) delete-change-set --stack-name $(STACK) --change-set-name $$cs; \
	  if [ $$type = CREATE ]; then $(CFN) delete-stack --stack-name $(STACK); fi; \
	  rm -f .last-change-set; \
	  case "$$reason" in \
	    *"didn't contain changes"*|*"No updates are to be performed"*) echo "no changes"; exit 0;; \
	    *) echo "FAIL: the change set could not be created"; exit 1;; \
	  esac; \
	fi; \
	$(CFN) describe-change-set --stack-name $(STACK) --change-set-name $$cs \
	  --query 'Changes[].ResourceChange.[Action,LogicalResourceId,ResourceType,Replacement]' --output table; \
	echo "$$cs $$type" > .last-change-set

# up: diff, then execute only after an explicit "yes" (CONFIRM=yes skips the prompt).
up: diff         ## deploy after confirming the printed change set
	@set -euo pipefail; \
	[ -f .last-change-set ] || { echo "nothing to execute"; exit 0; }; \
	read -r cs type < .last-change-set; rm -f .last-change-set; \
	if [ "$${CONFIRM:-}" != yes ]; then read -r -p "Execute $$cs? [yes/no] " ans; [ "$$ans" = yes ] || { echo aborted; exit 1; }; fi; \
	$(CFN) execute-change-set --stack-name $(STACK) --change-set-name $$cs; \
	if [ $$type = CREATE ]; then w=stack-create-complete; else w=stack-update-complete; fi; \
	$(CFN) wait $$w --stack-name $(STACK); \
	$(CFN) describe-stacks --stack-name $(STACK) --query 'Stacks[0].StackStatus' --output text

# down: the app stack imports the network's exports, so it goes first (ADR-0003). The
# artifacts stack is long-lived and stays (its bucket is kept anyway: DeletionPolicy Retain).
down:            ## delete the app stack (if any), then the network stack
	@set -euo pipefail; \
	if [ "$(STACK)" != "$(APP_STACK)" ] && $(CFN) describe-stacks --stack-name $(APP_STACK) >/dev/null 2>&1; then \
	  $(CFN) delete-stack --stack-name $(APP_STACK); \
	  $(CFN) wait stack-delete-complete --stack-name $(APP_STACK); \
	  echo "$(APP_STACK) deleted"; \
	fi; \
	$(CFN) delete-stack --stack-name $(STACK); \
	$(CFN) wait stack-delete-complete --stack-name $(STACK); \
	echo "$(STACK) deleted"

drift:           ## compare the stack's recorded state with what really exists
	@set -euo pipefail; \
	id=$$($(CFN) detect-stack-drift --stack-name $(STACK) --query StackDriftDetectionId --output text); \
	while [ "$$($(CFN) describe-stack-drift-detection-status --stack-drift-detection-id $$id --query DetectionStatus --output text)" = DETECTION_IN_PROGRESS ]; do sleep 3; done; \
	$(CFN) describe-stack-resource-drifts --stack-name $(STACK) \
	  --query 'StackResourceDrifts[].[LogicalResourceId,StackResourceDriftStatus]' --output table

# verify: drift detection can't see routes added outside the stack, so check the private
# route tables directly: none may have a 0.0.0.0/0 route (ADR-0005).
verify:          ## fail if any private route table has a route to the internet
	@set -euo pipefail; \
	rts=$$($(CFN) describe-stack-resources --stack-name $(STACK) \
	  --query "StackResources[?starts_with(LogicalResourceId,'PrivateRouteTable')].PhysicalResourceId" --output text); \
	bad=$$($(AWS) ec2 describe-route-tables --route-table-ids $$rts \
	  --query "RouteTables[?Routes[?DestinationCidrBlock=='0.0.0.0/0']].RouteTableId" --output text); \
	if [ -n "$$bad" ]; then echo "FAIL: internet route in $$bad"; exit 1; fi; \
	echo "OK: no private route table has 0.0.0.0/0"

rebuild:         ## timed teardown + rebuild (for docs/m1/recreate-log.md)
	@set -euo pipefail; \
	start=$$(date +%s); \
	$(MAKE) --no-print-directory down; \
	mid=$$(date +%s); \
	CONFIRM=yes $(MAKE) --no-print-directory up; \
	end=$$(date +%s); \
	echo "teardown: $$((mid-start))s  rebuild: $$((end-mid))s  total: $$((end-start))s"

# --- The Go app, locally (M2, M3) ----------------------------------------------------------

test:            ## go vet + all Go tests (incl. contract test and ring distribution)
	go vet ./... && go test ./...

keys:            ## make local API keys in secrets/ (git-ignored), unless they exist (ADR-0027)
	@scripts/new-api-keys.sh secrets
	@mkdir -p secrets/telegram   # telegram_token, telegram_chat_id: see docs/m4/runbook.md

local-up: keys   ## build the image and start coordinator + 2 shards x 3 replicas in Docker
	docker compose up --build -d

local-down:      ## stop the local cluster (add -v to docker compose down to wipe data)
	docker compose down

# A replica whose log is damaged in the middle refuses to start (ADR-0018). Rebuild it:
# remove its container and volume, start it empty, and it pulls the leader's whole log
# (ADR-0023 step 6). Never for a shard's only live copy.
rebuild-replica: ## wipe one local replica and resync it from its leader (R=1b)
	@test -n "$(R)" || { echo "usage: make rebuild-replica R=<replica, e.g. 1b>"; exit 1; }
	docker compose rm --stop --force r$(R)
	docker volume rm kv-r$(R)-data
	docker compose up -d r$(R)

smoke:           ## the four operations against a running coordinator (BASE=http://...)
	KV_API_KEY=$$(cat secrets/ops.key 2>/dev/null) scripts/smoke.sh $(or $(BASE),http://localhost:8080)

# --- M2: releases and the app in AWS (ADR-0009, ADR-0014) --------------------------------

BUCKET = $(shell $(CFN) list-exports --query "Exports[?Name=='$(ARTIFACTS_STACK)-BucketName'].Value" --output text)
comma := ,
CLIENT_CIDR ?= $(shell curl -s https://checkip.amazonaws.com)/32

up-artifacts:    ## create/update the S3 artifacts stack (once; it stays)
	$(MAKE) --no-print-directory up STACK=$(ARTIFACTS_STACK) TEMPLATE=infra/artifacts.yaml

# release: build the amd64 image of the current commit and stream it, with the boot script,
# to s3://<bucket>/releases/<sha>/. Refuses uncommitted app changes: a release must match
# a commit, or AppVersion=<sha> would lie about what runs.
release:         ## build + upload the image of the current commit (VERSION=<sha>)
	@set -euo pipefail; \
	if [ -n "$$(git status --porcelain -- cmd internal go.mod go.sum Dockerfile scripts/node-boot.sh scripts/smoke.sh scripts/smoke-replica.sh)" ]; then \
	  echo "uncommitted changes in app files: commit first, a release is named by its commit"; exit 1; fi; \
	[ -n "$(BUCKET)" ] || { echo "no artifacts bucket: run make up-artifacts"; exit 1; }; \
	if $(AWS) s3 ls s3://$(BUCKET)/releases/$(VERSION)/kvnode.tar.gz >/dev/null 2>&1; then \
	  echo "release $(VERSION) already in S3, nothing to build"; exit 0; fi; \
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) -t capstone-kv:$(VERSION) .; \
	docker save capstone-kv:$(VERSION) | gzip | $(AWS) s3 cp - s3://$(BUCKET)/releases/$(VERSION)/kvnode.tar.gz; \
	$(AWS) s3 cp scripts/node-boot.sh s3://$(BUCKET)/releases/$(VERSION)/node-boot.sh; \
	$(AWS) s3 cp scripts/smoke.sh s3://$(BUCKET)/releases/$(VERSION)/smoke.sh; \
	$(AWS) s3 cp scripts/smoke-replica.sh s3://$(BUCKET)/releases/$(VERSION)/smoke-replica.sh; \
	echo "released $(VERSION) to s3://$(BUCKET)/releases/$(VERSION)/"

BENCH ?= false   # BENCH=true adds the benchmark client instance to the app stack
EXPECTED_SHARDS ?= 2
W ?= 2           # write quorum (ADR-0021): 2, or 1 for the benchmark's W = 1 run
# The AMI is pinned per stack (ADR-0017). IMAGE_ID=ami-... upgrades it deliberately (the
# change set then shows every instance being replaced); otherwise a new stack gets the
# current Amazon Linux 2023 ID once, and an existing stack keeps the one it has.
IMAGE_ID ?=
AMI_PARAMETER := /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64

up-app:          ## deploy the app stack running VERSION (BENCH=true adds the client)
	@set -euo pipefail; \
	$(AWS) s3 ls s3://$(BUCKET)/releases/$(VERSION)/kvnode.tar.gz >/dev/null || \
	  { echo "release $(VERSION) not in S3: run make release"; exit 1; }; \
	status=$$($(CFN) describe-stacks --stack-name $(APP_STACK) --query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo NONE); \
	if [ -n "$(IMAGE_ID)" ]; then image="ParameterKey=ImageId,ParameterValue=$(IMAGE_ID)"; \
	elif [ "$$status" != NONE ] && [ "$$status" != REVIEW_IN_PROGRESS ]; then image="ParameterKey=ImageId,UsePreviousValue=true"; \
	else image="ParameterKey=ImageId,ParameterValue=$$($(AWS) ssm get-parameter --name $(AMI_PARAMETER) --query Parameter.Value --output text)"; fi; \
	echo "AMI: $$image"; \
	$(MAKE) --no-print-directory up STACK=$(APP_STACK) TEMPLATE=infra/app.yaml \
	  PARAMS="ParameterKey=AppVersion,ParameterValue=$(VERSION) $(if $(strip $(CLIENT_CIDR)),ParameterKey=AllowedClientCidr$(comma)ParameterValue=$(strip $(CLIENT_CIDR))) ParameterKey=BenchmarkClient,ParameterValue=$(strip $(BENCH)) ParameterKey=ExpectedShards,ParameterValue=$(EXPECTED_SHARDS) ParameterKey=W,ParameterValue=$(strip $(W)) $$image"

down-app:        ## delete the app stack (end of every lab session: it costs money)
	@set -euo pipefail; \
	$(CFN) delete-stack --stack-name $(APP_STACK); \
	$(CFN) wait stack-delete-complete --stack-name $(APP_STACK); \
	echo "$(APP_STACK) deleted"

# smoke-aws: run scripts/smoke.sh ON the coordinator through SSM Run Command, against
# localhost. Nothing has to be opened to the internet (the CI runner's address is unknown),
# and it tests the deployed release, not the laptop's copy of the script (ADR-0015).
# It checks VERSION, the release this run meant to deploy (CI passes the rollback version
# when there is one), never the version the stack reports: after a deploy that silently
# did nothing, the stack still reports the old version and the check would pass.
smoke-aws:       ## smoke test on the coordinator via SSM: every node must run VERSION
	@set -euo pipefail; \
	id=$$($(CFN) describe-stacks --stack-name $(APP_STACK) --query "Stacks[0].Outputs[?OutputKey=='CoordinatorInstanceId'].OutputValue" --output text); \
	ver=$(VERSION); \
	echo "coordinator $$id, expected release $$ver: waiting for the SSM agent"; \
	for i in $$(seq 30); do \
	  [ "$$($(AWS) ssm describe-instance-information --filters Key=InstanceIds,Values=$$id --query 'InstanceInformationList[0].PingStatus' --output text)" = Online ] && break; \
	  sleep 10; done; \
	cmd=$$($(AWS) ssm send-command --instance-ids $$id --document-name AWS-RunShellScript \
	  --comment "capstone-kv smoke $$ver" \
	  --parameters "commands=[\"aws s3 cp s3://$(BUCKET)/releases/$$ver/smoke.sh /tmp/smoke.sh --region $(REGION)\",\"export KV_API_KEY=\$$(aws secretsmanager get-secret-value --secret-id velmakina-kv/ops-key --query SecretString --output text --region $(REGION))\",\"bash /tmp/smoke.sh http://localhost:8080 $$ver\"]" \
	  --query Command.CommandId --output text); \
	$(AWS) ssm wait command-executed --command-id $$cmd --instance-id $$id || true; \
	$(AWS) ssm get-command-invocation --command-id $$cmd --instance-id $$id --query StandardOutputContent --output text; \
	status=$$($(AWS) ssm get-command-invocation --command-id $$cmd --instance-id $$id --query Status --output text); \
	echo "SSM status: $$status"; [ "$$status" = Success ]

# smoke-replica (M3 task 7.1): stop a shard node that runs no leader, so one follower of each
# shard is down; check through the coordinator that puts still commit and read back; start
# the node and wait until its replicas have caught up. The checks are scripts/smoke-replica.sh
# of the deployed release, run on the coordinator through SSM, as in smoke-aws. The nodes
# have no SSM route, so the whole instance is stopped. It is started again even when a check
# fails (trap). A release from before M3 has no such script: skipped.
smoke-replica:   ## smoke test with one follower per shard down (stops and restarts a node)
	@set -euo pipefail; \
	coord=$$($(CFN) describe-stacks --stack-name $(APP_STACK) --query "Stacks[0].Outputs[?OutputKey=='CoordinatorInstanceId'].OutputValue" --output text); \
	ver=$(VERSION); script=s3://$(BUCKET)/releases/$$ver/smoke-replica.sh; \
	$(AWS) s3 ls $$script >/dev/null 2>&1 || { echo "release $$ver has no smoke-replica.sh (before M3): skipped"; exit 0; }; \
	onCoord() { \
	  cmd=$$($(AWS) ssm send-command --instance-ids $$coord --document-name AWS-RunShellScript \
	    --comment "capstone-kv smoke-replica $$ver $$1" \
	    --parameters "commands=[\"aws s3 cp $$script /tmp/smoke-replica.sh --quiet --region $(REGION)\",\"export KV_API_KEY=\$$(aws secretsmanager get-secret-value --secret-id velmakina-kv/ops-key --query SecretString --output text --region $(REGION))\",\"bash /tmp/smoke-replica.sh http://localhost:8080 $$*\"]" \
	    --query Command.CommandId --output text); \
	  for i in $$(seq 100); do \
	    st=$$($(AWS) ssm get-command-invocation --command-id $$cmd --instance-id $$coord --query Status --output text 2>/dev/null || echo Pending); \
	    case $$st in Pending|InProgress|Delayed) sleep 3;; *) break;; esac; done; \
	  $(AWS) ssm get-command-invocation --command-id $$cmd --instance-id $$coord --query StandardOutputContent --output text; \
	  [ "$$st" = Success ] || { echo "SSM status: $$st" >&2; return 1; }; }; \
	host=$$(onCoord pick); \
	node=$$($(AWS) ec2 describe-instances --filters Name=private-ip-address,Values=$$host \
	  Name=tag:aws:cloudformation:stack-name,Values=$(APP_STACK) Name=instance-state-name,Values=running \
	  --query "Reservations[0].Instances[0].InstanceId" --output text); \
	case $$node in i-*) ;; *) echo "FAIL no running node at $$host"; exit 1;; esac; \
	started=no; \
	start() { [ $$started = yes ] && return 0; started=yes; echo "starting $$node"; \
	  $(AWS) ec2 start-instances --instance-ids $$node >/dev/null; \
	  $(AWS) ec2 wait instance-running --instance-ids $$node; }; \
	trap start EXIT; \
	echo "stopping node $$node ($$host): one follower of each shard"; \
	$(AWS) ec2 stop-instances --instance-ids $$node >/dev/null; \
	$(AWS) ec2 wait instance-stopped --instance-ids $$node; \
	onCoord down $$host; \
	start; \
	onCoord up $$host; \
	echo "smoke-replica passed"

# secrets (lab session, a write call): creates or updates the three API-key secrets from
# the files `make keys` made (ADR-0027). Values go through file:// so they never appear on
# a command line or in this output.
secrets:         ## create/update velmakina-kv/api-keys, ops-key, probe-key from secrets/ (author approves)
	@set -euo pipefail; \
	for pair in api-keys:secrets/api-keys.json ops-key:secrets/ops.key probe-key:secrets/probe.key; do \
	  name=velmakina-kv/$${pair%%:*} file=$${pair#*:}; \
	  test -s "$$file" || { echo "$$file missing: run make keys"; exit 1; }; \
	  if $(AWS) secretsmanager describe-secret --secret-id $$name >/dev/null 2>&1; then \
	    $(AWS) secretsmanager put-secret-value --secret-id $$name --secret-string file://$$file >/dev/null; echo "updated $$name"; \
	  else \
	    $(AWS) secretsmanager create-secret --name $$name --secret-string file://$$file --tags Key=Project,Value=capstone-kv >/dev/null; echo "created $$name"; \
	  fi; \
	done

# rotate-keys: after `make secrets` with changed keys, refresh the coordinator's keys file
# through SSM; the coordinator reloads it within seconds, no restart (ADR-0027).
rotate-keys:     ## refresh the coordinator's API keys from Secrets Manager
	@set -euo pipefail; \
	id=$$($(CFN) describe-stacks --stack-name $(APP_STACK) --query "Stacks[0].Outputs[?OutputKey=='CoordinatorInstanceId'].OutputValue" --output text); \
	f=/var/lib/kv/coordinator/keys/api-keys.json; \
	cmd=$$($(AWS) ssm send-command --instance-ids $$id --document-name AWS-RunShellScript --comment "capstone-kv rotate keys" \
	  --parameters "commands=[\"aws secretsmanager get-secret-value --secret-id velmakina-kv/api-keys --query SecretString --output text --region $(REGION) > $$f.new\",\"chown 65532:65532 $$f.new\",\"mv $$f.new $$f\"]" \
	  --query Command.CommandId --output text); \
	$(AWS) ssm wait command-executed --command-id $$cmd --instance-id $$id; \
	echo "keys file refreshed on $$id"

# bench-aws: needs the app stack with BENCH=true. Builds kvbench for the client (amd64),
# uploads it with the workload files and scripts/bench.sh to s3://<bucket>/bench/<sha>/,
# runs bench.sh on the client through SSM and waits (polling: a full run takes ~40 min,
# longer than the CLI's built-in waiter). The client can't write to S3 (instance role, M1
# evidence 63), so the summaries come back in the SSM output; this target saves them to
# bench-results/<sha>/<time>.txt and uploads that file to S3 with its own credentials.
RUNS ?= 3
WORKLOADS ?= a b # M3 variants: WORKLOADS=a LABEL=w1 (or LABEL=follower-down)
LABEL ?=

bench-aws:       ## YCSB A and B on the client in AWS (app stack up with BENCH=true)
	@set -euo pipefail; \
	out() { $(CFN) describe-stacks --stack-name $(APP_STACK) --query "Stacks[0].Outputs[?OutputKey=='$$1'].OutputValue" --output text; }; \
	client=$$(out BenchmarkClientInstanceId); url=$$(out CoordinatorPrivateUrl); \
	case "$$client" in i-*) ;; *) echo "no benchmark client: run make up-app BENCH=true"; exit 1;; esac; \
	(cd bench && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o ../.kvbench-linux .); \
	$(AWS) s3 cp .kvbench-linux s3://$(BUCKET)/bench/$(VERSION)/kvbench --quiet; \
	$(AWS) s3 cp --recursive bench/workloads s3://$(BUCKET)/bench/$(VERSION)/workloads --quiet; \
	$(AWS) s3 cp scripts/bench.sh s3://$(BUCKET)/bench/$(VERSION)/bench.sh --quiet; \
	echo "client $$client -> $$url, $(RUNS) runs per workload: waiting for the SSM agent"; \
	for i in $$(seq 30); do \
	  [ "$$($(AWS) ssm describe-instance-information --filters Key=InstanceIds,Values=$$client --query 'InstanceInformationList[0].PingStatus' --output text)" = Online ] && break; \
	  sleep 10; done; \
	cmd=$$($(AWS) ssm send-command --instance-ids $$client --document-name AWS-RunShellScript \
	  --comment "capstone-kv benchmark $(VERSION)" --timeout-seconds 600 \
	  --parameters "executionTimeout=[\"7200\"],commands=[\"aws s3 cp s3://$(BUCKET)/bench/$(VERSION)/bench.sh /tmp/bench.sh --region $(REGION)\",\"bash /tmp/bench.sh $(BUCKET) $(VERSION) $$url $(RUNS) '$(strip $(WORKLOADS))'\"]" \
	  --query Command.CommandId --output text); \
	echo "SSM command $$cmd started $$(date -u +%T) UTC; polling every 30 s"; \
	while :; do \
	  status=$$($(AWS) ssm get-command-invocation --command-id $$cmd --instance-id $$client --query Status --output text 2>/dev/null || echo Pending); \
	  case $$status in Pending|InProgress|Delayed) sleep 30;; *) break;; esac; \
	done; \
	result=bench-results/$(VERSION)/$$(date -u +%Y%m%dT%H%M%SZ)$(if $(LABEL),-$(LABEL)).txt; mkdir -p bench-results/$(VERSION); \
	{ echo "release $(VERSION), client $$client -> $$url, workloads: $(strip $(WORKLOADS)), runs per workload: $(RUNS), label: $(or $(LABEL),-), SSM status: $$status"; \
	  $(AWS) ssm get-command-invocation --command-id $$cmd --instance-id $$client --query StandardOutputContent --output text; } > $$result; \
	cat $$result; \
	$(AWS) s3 cp $$result s3://$(BUCKET)/$$result --quiet; \
	echo "saved to $$result and s3://$(BUCKET)/$$result"; \
	[ "$$status" = Success ]

app-url:         ## print the coordinator URL of the running app stack
	@$(CFN) describe-stacks --stack-name $(APP_STACK) \
	  --query "Stacks[0].Outputs[?OutputKey=='CoordinatorUrl'].OutputValue" --output text

