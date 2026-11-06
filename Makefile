# capstone-kv M1: network stack lifecycle (design.md decision 8, infra-lifecycle spec).
# Every update goes through a change set that is printed before anything changes.
# Written for GNU Make 3.81 (macOS default): no .ONESHELL, so multi-line recipes are
# joined with "; \" and run in one shell.

SHELL    := /bin/bash
PROFILE  ?= academy
REGION   ?= us-east-1
STACK    ?= velmakina-kv-network
TEMPLATE := infra/network.yaml
AWS      := aws --profile $(PROFILE) --region $(REGION)
CFN      := $(AWS) cloudformation

.PHONY: help whoami lint diff up down drift verify rebuild

help:            ## list targets
	@grep -E '^[a-z]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*##/ —/'

whoami:          ## which AWS identity is active
	@$(AWS) sts get-caller-identity --query Arn --output text

lint:            ## static checks on both templates (no AWS calls)
	cfn-lint infra/*.yaml

# diff: create a change set WITHOUT executing it and print what it would do.
# CREATE if the stack doesn't exist (or only ever had an unexecuted change set), else UPDATE.
diff:            ## show what a deploy would change
	@set -euo pipefail; \
	status=$$($(CFN) describe-stacks --stack-name $(STACK) --query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo NONE); \
	if [ "$$status" = NONE ] || [ "$$status" = REVIEW_IN_PROGRESS ]; then type=CREATE; else type=UPDATE; fi; \
	cs=$(STACK)-$$(date +%Y%m%d-%H%M%S); \
	$(CFN) create-change-set --stack-name $(STACK) --change-set-name $$cs --change-set-type $$type \
	  --template-body file://$(TEMPLATE) --tags Key=Project,Value=capstone-kv >/dev/null; \
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

# down: app stacks that import our exports must be deleted first (ADR-0003). None exist in M1.
down:            ## delete the network stack
	@set -euo pipefail; \
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
