# Stream Disturbance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a "Send 1000 Packets" button to MainPage that streams packets to the backend via the `StreamDisturbance` gRPC client-streaming RPC, showing live progress.

**Architecture:** A new `useStreamDisturbance` hook owns all stream state (progress, streaming flag, result, error) and calls `client.streamDisturbance()` with an async generator that yields 1000 `DataPacket` messages. `MainPage` consumes both `useSendPacket` (existing) and `useStreamDisturbance` (new), passing `clientId` and `payload` from the form into the stream hook.

**Tech Stack:** React, TypeScript, `@connectrpc/connect` client-streaming API, `@bufbuild/protobuf`

---

## File Map

| Action | Path | Responsibility |
|--------|------|----------------|
| Create | `src/components/main-page/useStreamDisturbance.ts` | Stream state + logic |
| Modify | `src/components/main-page/MainPage.tsx` | Render stress-test section |

---

### Task 1: Create `useStreamDisturbance` hook

**Files:**
- Create: `src/components/main-page/useStreamDisturbance.ts`

- [ ] **Step 1: Create the hook file**

```ts
import { useState } from 'react';
import { client } from '../../client';

interface StreamResult {
  message: string;
  success: boolean;
}

function useStreamDisturbance(clientId: string, payload: string) {
  const [progress, setProgress] = useState(0);
  const [streaming, setStreaming] = useState(false);
  const [result, setResult] = useState<StreamResult | null>(null);
  const [error, setError] = useState<string | null>(null);

  async function startStream() {
    if (streaming) return;

    setStreaming(true);
    setProgress(0);
    setResult(null);
    setError(null);

    try {
      const res = await client.streamDisturbance(async function* () {
        for (let i = 0; i < 1000; i++) {
          yield { clientId, payload, sequenceNumber: i };
          setProgress(i + 1);
        }
      });
      setResult({ message: res.message, success: res.success });
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unknown error');
    } finally {
      setStreaming(false);
    }
  }

  return { progress, streaming, result, error, startStream };
}

export default useStreamDisturbance;
```

- [ ] **Step 2: Type-check**

Run from `frontend/`:
```
npx tsc --noEmit
```
Expected: no errors.

- [ ] **Step 3: Commit**

```
git add src/components/main-page/useStreamDisturbance.ts
git commit -m "feat: add useStreamDisturbance hook for client-streaming RPC"
```

---

### Task 2: Update `MainPage` to render stress-test section

**Files:**
- Modify: `src/components/main-page/MainPage.tsx`

- [ ] **Step 1: Update MainPage.tsx**

Replace the full file content with:

```tsx
import useSendPacket from './useSendPacket';
import useStreamDisturbance from './useStreamDisturbance';
import '../../App.css';

function MainPage() {
  const { form, response, loading, error, handleChange, handleSubmit } =
    useSendPacket();

  const {
    progress,
    streaming,
    result: streamResult,
    error: streamError,
    startStream,
  } = useStreamDisturbance(form.clientId, form.payload);

  return (
    <section id="center">
      <h1>Send Packet</h1>
      <form className="packet-form" onSubmit={handleSubmit}>
        <label>
          Client ID
          <input
            name="clientId"
            type="text"
            value={form.clientId}
            onChange={handleChange}
            placeholder="client-001"
            required
          />
        </label>
        <label>
          Payload
          <input
            name="payload"
            type="text"
            value={form.payload}
            onChange={handleChange}
            placeholder="hello world"
            required
          />
        </label>
        <label>
          Sequence Number
          <input
            name="sequenceNumber"
            type="number"
            value={form.sequenceNumber}
            onChange={handleChange}
            min={0}
            required
          />
        </label>
        <button type="submit" disabled={loading}>
          {loading ? 'Sending…' : 'Send Packet'}
        </button>
      </form>

      {response && (
        <div className={`response ${response.success ? 'success' : 'failure'}`}>
          <strong>{response.success ? 'Success' : 'Failed'}</strong>
          <span>{response.message}</span>
        </div>
      )}

      {error && (
        <div className="response failure">
          <strong>Error</strong>
          <span>{error}</span>
        </div>
      )}

      <div className="stress-test">
        <button onClick={startStream} disabled={streaming}>
          {streaming ? 'Streaming…' : 'Send 1000 Packets'}
        </button>

        {(streaming || streamResult || streamError) && (
          <p className="progress">Sent {progress} / 1000</p>
        )}

        {streamResult && (
          <div className={`response ${streamResult.success ? 'success' : 'failure'}`}>
            <strong>{streamResult.success ? 'Success' : 'Failed'}</strong>
            <span>{streamResult.message}</span>
          </div>
        )}

        {streamError && (
          <div className="response failure">
            <strong>Error</strong>
            <span>{streamError}</span>
          </div>
        )}
      </div>
    </section>
  );
}

export default MainPage;
```

- [ ] **Step 2: Type-check**

Run from `frontend/`:
```
npx tsc --noEmit
```
Expected: no errors.

- [ ] **Step 3: Manual verify**

Run `npm run dev` from `frontend/`. Open browser.
- Fill in Client ID and Payload fields.
- Click "Send 1000 Packets".
- Confirm button shows "Streaming…" and is disabled.
- Confirm `Sent X / 1000` counter increments live.
- Confirm final result block appears after stream completes.
- Confirm "Send Packet" form still works independently.

- [ ] **Step 4: Commit**

```
git add src/components/main-page/MainPage.tsx
git commit -m "feat: render stream disturbance section in MainPage"
```
