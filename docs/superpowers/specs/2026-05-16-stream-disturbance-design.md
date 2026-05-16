# Stream Disturbance Feature Design

## Overview

Add a "Send 1000 Packets" button to the main page that stress-tests the backend by streaming 1000 packets via the existing `StreamDisturbance` gRPC client-streaming RPC. Live progress is shown during the stream.

## Architecture

Two files added inside `src/components/main-page/`:

- `useStreamDisturbance.ts` — hook owning all stream state and logic
- `MainPage.tsx` — updated to render the new stress-test section

No new pages, no routing changes, no changes to existing `useSendPacket.ts`.

## Components

### `useStreamDisturbance.ts`

Accepts `clientId: string` and `payload: string` as arguments (sourced from the existing form state in `MainPage`).

State:
- `progress: number` — packets sent so far (0–1000)
- `streaming: boolean` — true while stream is active
- `result: { message: string; success: boolean } | null` — final response from server
- `error: string | null`

Logic:
- `startStream()` — opens `client.streamDisturbance()`, sends 1000 `DataPacket` messages with `sequenceNumber` 0–999, increments `progress` after each send, closes stream, sets `result` from server response or sets `error` on failure.
- Button is disabled and `startStream` is a no-op while `streaming` is true.

### `MainPage.tsx`

- Passes `form.clientId` and `form.payload` into `useStreamDisturbance`.
- Renders a second section below the existing form with:
  - "Send 1000 Packets" button (disabled while `streaming`)
  - Progress display: `Sent X / 1000` (visible while `streaming` or after completion)
  - Final result or error block (same styling as existing response/error blocks)

## Data Flow

```
MainPage
  └── useSendPacket        (existing, unchanged)
  └── useStreamDisturbance (new)
        └── client.streamDisturbance() → StreamDisturbance RPC
```

Form fields `clientId` and `payload` flow from `useSendPacket`'s `form` state into `useStreamDisturbance` as args. `sequenceNumber` auto-increments 0–999 inside the hook.

## Error Handling

- Network/stream error caught in try/catch, sets `error` state, clears `streaming`.
- `progress` and `result` reset to initial state on each new `startStream()` call.

## Out of Scope

- Cancel mid-stream
- Configurable packet count
- Per-packet response handling (server returns one response at end of client stream)
