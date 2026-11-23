# Spec Delta

## Purpose

Everything can be destroyed and rebuilt from the repository alone, changes are reviewed
before they run, and the rebuild is timed and logged as evidence.

## ADDED Requirements

### Requirement: One-command up and down
The repo SHALL have one command to deploy the network stack and one to delete it, using
only committed templates and parameters.

#### Scenario: Bring-up
- **WHEN** the up command runs in a region without the stack
- **THEN** the stack reaches `CREATE_COMPLETE` with no console steps

#### Scenario: Teardown
- **WHEN** the down command runs
- **THEN** the stack reaches `DELETE_COMPLETE` and no VPC tagged `Project=capstone-kv` remains

### Requirement: Rebuild is timed and logged
The repo SHALL contain a log of a full teardown and rebuild, with timings and every
first-attempt failure.

#### Scenario: Log recorded
- **WHEN** a reader opens the recreate log
- **THEN** it shows date, region, teardown time, rebuild time, and each failure with cause and fix

### Requirement: Changes are shown before they run
Every stack update SHALL first create a change set without executing it, print its
resource actions including Replacement, and execute only on explicit confirmation.

#### Scenario: Change set reviewed
- **WHEN** a template change is deployed to an existing stack
- **THEN** the change set's actions are printed and nothing changes until it is confirmed

### Requirement: Drift and route checks
Drift SHALL be checkable on demand, and a separate check SHALL verify the private route
tables directly, since drift detection doesn't see routes added outside the stack.

#### Scenario: Drift check
- **WHEN** the drift command runs
- **THEN** each resource is reported as IN_SYNC, MODIFIED, DELETED or NOT_CHECKED

#### Scenario: Route check
- **WHEN** the verify command runs
- **THEN** it fails if any private route table has a `0.0.0.0/0` route, however it was added
