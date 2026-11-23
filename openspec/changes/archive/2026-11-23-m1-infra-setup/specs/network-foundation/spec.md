# Spec Delta

## Purpose

The network every later milestone deploys into: public and internal-only subnets across
availability zones, plus exported IDs so app stacks can find them.

## ADDED Requirements

### Requirement: Dedicated VPC
The infrastructure SHALL create one VPC with the CIDR from ADR-0004 and DNS support and
DNS hostnames enabled. Every resource SHALL carry the tag `Project=capstone-kv`.

#### Scenario: VPC exists
- **WHEN** the network stack is deployed
- **THEN** one VPC tagged `Project=capstone-kv` exists with that CIDR and both DNS settings true

### Requirement: Subnets in three AZs
The VPC SHALL have one public and one private subnet in each of the three AZ IDs given as
parameters, using the CIDRs from ADR-0004.

#### Scenario: Six subnets
- **WHEN** the VPC's subnets are listed
- **THEN** there are three public and three private subnets, one of each per given AZ ID

### Requirement: Public subnets route to the internet
Public subnets SHALL route `0.0.0.0/0` to the VPC's Internet Gateway.

#### Scenario: Public default route
- **WHEN** a public subnet's route table is inspected
- **THEN** `0.0.0.0/0` points to the Internet Gateway

### Requirement: Private subnets are internal-only
Private subnets SHALL have no `0.0.0.0/0` route and SHALL NOT assign public IPs.

#### Scenario: No default route
- **WHEN** a private route table is inspected
- **THEN** it has only the local route and the S3 gateway-endpoint route

#### Scenario: No public IPs
- **WHEN** a private subnet's attributes are inspected
- **THEN** `MapPublicIpOnLaunch` is false

### Requirement: S3 without internet
The VPC SHALL have an S3 gateway endpoint attached to every private route table.

#### Scenario: Endpoint present
- **WHEN** the VPC's endpoints are listed
- **THEN** a gateway endpoint for S3 exists and is attached to every private route table

### Requirement: Exported IDs, one value per export
The stack SHALL export the VPC ID, VPC CIDR, each subnet ID and each AZ ID as a separate
export prefixed with the stack name. No export SHALL hold a list.

#### Scenario: Exports available
- **WHEN** `aws cloudformation list-exports` is run
- **THEN** there are exports for the VPC ID and CIDR, six subnet IDs and three AZ IDs, each holding a single value
