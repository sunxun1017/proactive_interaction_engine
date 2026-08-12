# ADR 0001: Modular Monolith with Ports and Adapters

- Status: Accepted
- Date: 2026-08-12

## Context

The product must run on a PC first and later support multiple robot platforms without coupling policy to hardware, model vendors, or ROS. The real-time interaction path must continue operating during network, model, or storage failures.

## Decision

Use one Go module and a modular-monolith core. Place pure domain rules at the center, application orchestration around them, and all platform concerns behind ports. Use Protobuf only at process boundaries and isolate model-heavy runtimes in separate workers.

## Consequences

- Replay and fake adapters can validate the full core without devices.
- Platform changes are adapter changes rather than policy rewrites.
- Core package boundaries and explicit mapping require more discipline.
- Independent deployment is deferred until measured operational needs justify it.
