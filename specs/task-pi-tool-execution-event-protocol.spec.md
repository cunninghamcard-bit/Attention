spec: task
name: "Pi Tool Execution Event Protocol"
inherits: project
tags: [phase-1, hook, rpc, parity]
---

## Intent

Complete Attention's tool-execution lifecycle protocol so
`tool_execution_update` and `tool_execution_end` are delivered to kernel hooks
before they are published to RPC and TUI subscribers. Hook handlers may
mutate the same event object, matching Pi's event-first model without adding a
separate patch-result protocol. This is a core Pi-compatible runtime contract
and is not plugin-specific.

## Decisions

- Keep `tool_execution_update` and `tool_execution_end` as plain hook events whose handler return values are ignored.
- Emit `ToolExecutionUpdateEvent` and `ToolExecutionEndEvent` as mutable event pointers from the harness bridge.
- Each handler receives the same event object after earlier handlers have applied field mutations.
- Handler errors are reported and dispatch continues to later handlers.
- The orchestrator mode publisher runs after shell hooks and native/user extension handlers so subscribers receive the final mutated event.
- Shell hooks remain notification-only for these lifecycle events and do not parse stdout as a JSON patch.
- `hook.Registry.Emit` remains generic and does not hardcode tool execution patch semantics.
- `tool_execution_start` remains notification-only.
- Do not add a second event bus.

## Boundaries

### Allowed Changes
- internal/hook/**
- internal/harness/**
- internal/agentloop/**
- internal/orchestrator/**
- internal/mode/rpc/**

### Forbidden
- Do not add plugin-specific files or behavior.
- Do not change slash-command execution.
- Do not add patch-result types for tool execution lifecycle events.
- Do not make unrelated notification events mutable.
- Do not special-case the TUI.

### Out of Scope
- RPC slash-command dispatch.
- File plugin system.
- Interactive settings UI.

## Completion Criteria

Scenario: update mutation is published to subscribers
  Test: TestCompletionCriteriaToolExecutionUpdateMutationPublishesFinalEvent
  Given a `tool_execution_update` handler mutates the event partial result
  When a tool emits a streaming partial result
  Then orchestrator subscribers receive the mutated partial result
  And RPC serialization uses the mutated partial result

Scenario: end mutation is published to subscribers
  Test: TestCompletionCriteriaToolExecutionEndMutationPublishesFinalEvent
  Given a `tool_execution_end` handler mutates the final result and error flag
  When a tool execution finishes
  Then orchestrator subscribers receive the mutated result
  And RPC serialization uses the mutated `isError` value

Scenario: multiple mutation handlers compose in order
  Test: TestCompletionCriteriaToolExecutionMutationHandlersComposeAndErrorsDoNotBlockPublish
  Given two `tool_execution_update` handlers mutate the same event object
  When the hook registry emits the update event
  Then the second handler observes the first handler's mutation
  And subscribers receive the second handler's final mutation

Scenario: handler error does not block later mutation or publish
  Test: TestCompletionCriteriaToolExecutionMutationHandlersComposeAndErrorsDoNotBlockPublish
  Given the first tool execution handler returns an error
  And a later handler applies a valid mutation
  When the hook registry emits the event
  Then the error is reported
  And subscribers still receive the later mutated event

Scenario: publisher runs after hook handlers
  Test: TestCompletionCriteriaToolExecutionPublisherRunsAfterNativeHandlers
  Given a hook handler mutates tool execution update and end events
  When the orchestrator publishes those lifecycle events
  Then subscribers receive the final mutated event state
  And no TUI-specific branch is required

Scenario: start event remains notification-only
  Test: TestEventSinkToolExecutionUpdateEndUseMutableEvents
  Given `tool_execution_start` is emitted as a value event
  When a tool execution starts
  Then the start event is still published unchanged
  And update and end events are the only mutable tool execution lifecycle events
