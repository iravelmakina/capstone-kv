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
AWS      := $(AWSCLI) --profile $(PROFILE) --region $(REGION)
CFN      := $(AWS) cloudformation

ARTIFACTS_STACK := velmakina-kv-artifacts
APP_STACK       := velmakina-kv-app
# A release is named by its commit.
VERSION  ?= $(shell git rev-parse --short=12 HEAD)
CFNLINT  := $(if $(wildcard .venv/bin/cfn-lint),.venv/bin/cfn-lint,cfn-lint)

.PHONY: help whoami lint diff up down drift verify rebuild test local-up local-down smoke \
        up-artifacts release up-app down-app app-url

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
	  $(CFN) describe-change-set --stack-name $(STACK) --change-set-name $$cs --query StatusReason --output text; \
	  $(CFN) delete-change-set --stack-name $(STACK) --change-set-name $$cs; \
	  if [ $$type = CREATE ]; then $(CFN) delete-stack --stack-name $(STACK); fi; \
	  rm -f .last-change-set; exit 0; \
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

# --- M2: the Go app, locally -------------------------------------------------------------

test:            ## go vet + all Go tests (incl. contract test and ring distribution)
	go vet ./... && go test ./...

local-up:        ## build the image and start coordinator + 2 shards in Docker
	docker compose up --build -d

local-down:      ## stop the local cluster (add -v to docker compose down to wipe data)
	docker compose down

smoke:           ## the four operations against a running coordinator (BASE=http://...)
	scripts/smoke.sh $(or $(BASE),http://localhost:8080)

# --- M2: releases and the app in AWS (ADR-0009, ADR-0014) --------------------------------

BUCKET = $(shell $(CFN) list-exports --query "Exports[?Name=='$(ARTIFACTS_STACK)-BucketName'].Value" --output text)
CLIENT_CIDR ?= $(shell curl -s https://checkip.amazonaws.com)/32

up-artifacts:    ## create/update the S3 artifacts stack (once; it stays)
	$(MAKE) --no-print-directory up STACK=$(ARTIFACTS_STACK) TEMPLATE=infra/artifacts.yaml

# release: build the amd64 image of the current commit and stream it, with the boot script,
# to s3://<bucket>/releases/<sha>/. Refuses uncommitted app changes: a release must match
# a commit, or AppVersion=<sha> would lie about what runs.
release:         ## build + upload the image of the current commit (VERSION=<sha>)
	@set -euo pipefail; \
	if [ -n "$$(git status --porcelain -- cmd internal go.mod go.sum Dockerfile scripts/node-boot.sh)" ]; then \
	  echo "uncommitted changes in app files: commit first, a release is named by its commit"; exit 1; fi; \
	[ -n "$(BUCKET)" ] || { echo "no artifacts bucket: run make up-artifacts"; exit 1; }; \
	docker build --platform linux/amd64 -t capstone-kv:$(VERSION) .; \
	docker save capstone-kv:$(VERSION) | gzip | $(AWS) s3 cp - s3://$(BUCKET)/releases/$(VERSION)/kvnode.tar.gz; \
	$(AWS) s3 cp scripts/node-boot.sh s3://$(BUCKET)/releases/$(VERSION)/node-boot.sh; \
	echo "released $(VERSION) to s3://$(BUCKET)/releases/$(VERSION)/"

up-app:          ## deploy the app stack running VERSION (default: current commit)
	@$(AWS) s3 ls s3://$(BUCKET)/releases/$(VERSION)/kvnode.tar.gz >/dev/null || \
	  { echo "release $(VERSION) not in S3: run make release"; exit 1; }
	$(MAKE) --no-print-directory up STACK=$(APP_STACK) TEMPLATE=infra/app.yaml \
	  PARAMS="ParameterKey=AppVersion,ParameterValue=$(VERSION) ParameterKey=AllowedClientCidr,ParameterValue=$(CLIENT_CIDR)"

down-app:        ## delete the app stack (end of every lab session: it costs money)
	@set -euo pipefail; \
	$(CFN) delete-stack --stack-name $(APP_STACK); \
	$(CFN) wait stack-delete-complete --stack-name $(APP_STACK); \
	echo "$(APP_STACK) deleted"

app-url:         ## print the coordinator URL of the running app stack
	@$(CFN) describe-stacks --stack-name $(APP_STACK) \
	  --query "Stacks[0].Outputs[?OutputKey=='CoordinatorUrl'].OutputValue" --output text

