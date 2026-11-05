# Spec Delta

## Purpose

A view-only permission set and a role another principal can assume to get it, written
as code, so read access never comes with write access or access to stored data.

## ADDED Requirements

### Requirement: View policy
The code SHALL define a managed policy that allows only List and Read actions on
resource configuration.

#### Scenario: No write actions allowed
- **WHEN** each allowed action is checked against its IAM access level
- **THEN** every action is List or Read; none is Write, Permissions management or Tagging

### Requirement: Separate deny policy for data and secrets
The code SHALL define a second managed policy that explicitly denies reading stored data
and secret values (list in ADR-0006).

#### Scenario: Data actions denied
- **WHEN** `simulate-custom-policy` evaluates both policies together against the data-action list in ADR-0006
- **THEN** every action on the list is `explicitDeny`

#### Scenario: Write denied
- **WHEN** the simulator evaluates `ec2:RunInstances` or `s3:PutObject`
- **THEN** the result is not `allowed`

#### Scenario: Viewing still works
- **WHEN** the simulator evaluates `ec2:DescribeVpcs` and `s3:ListAllMyBuckets`
- **THEN** both are `allowed`

### Requirement: Assumable role
The code SHALL define a role with both policies attached and a one-hour maximum session,
trusting only one IAM user or role passed as a parameter, and only with MFA.

#### Scenario: Trust policy
- **WHEN** the role's trust policy is read
- **THEN** it allows `sts:AssumeRole` only for the parameter principal, with `Bool` `aws:MultiFactorAuthPresent` = true and `aws:MultiFactorAuthAge` ≤ 3600

#### Scenario: Whole-account principal rejected
- **WHEN** the template is given an account root ARN (`arn:aws:iam::<id>:root`) as the principal
- **THEN** parameter validation rejects it

### Requirement: Linted, not deployed
The template SHALL pass `cfn-lint`, and the docs SHALL record the sandbox's refusal to create it.

#### Scenario: Lint passes
- **WHEN** `cfn-lint infra/iam-readonly.yaml` is run
- **THEN** it reports no errors

#### Scenario: Refusal recorded
- **WHEN** a reader opens the IAM section of the M1 docs
- **THEN** it quotes the AccessDenied message and names the policy layer that produced it
