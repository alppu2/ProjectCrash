import { useCallback, useEffect, useRef, useState } from 'react';
import { chatClient } from '../../api';
import { Role } from '../../gen/chat_pb';

export interface ChatMessage {
  role: Role;
  content: string;
}

// The server is stateless, so the client owns the history.
export const MAX_HISTORY = 20;

// Kept under chat.go's maxHistoryBytes (32768). The message cap alone is not
// enough: 20 messages of model output crosses 32KB after ~10 exchanges, and
// past that point every send fails InvalidArgument for the rest of the session
// because the rollback restores the same oversized history.
export const MAX_HISTORY_BYTES = 24000;

// Bytes, not UTF-16 code units, to match the server's len(content).
function byteLength(text: string): number {
  return new TextEncoder().encode(text).length;
}

// The window has to begin on a user turn, so snapping forward can return fewer
// messages than either cap allows. Exported for tests: both caps mirror
// chat.go, and a mismatch is invisible until a long conversation fails.
export function trimHistory(history: ChatMessage[]): ChatMessage[] {
  let start = Math.max(0, history.length - MAX_HISTORY);
  let bytes = history
    .slice(start)
    .reduce((total, m) => total + byteLength(m.content), 0);

  // Always keep the last message: it is the turn being sent, and a single
  // oversized message is the server's to reject.
  while (bytes > MAX_HISTORY_BYTES && start < history.length - 1) {
    bytes -= byteLength(history[start].content);
    start += 1;
  }

  while (start < history.length && history[start].role !== Role.USER) {
    start += 1;
  }
  return history.slice(start);
}

function useChatStream() {
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [streaming, setStreaming] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  // A ref, not the `streaming` state: two overlapping send() calls would both
  // read a stale streaming === false before either setStreaming(true) lands.
  const streamingRef = useRef(false);

  useEffect(() => () => abortRef.current?.abort(), []);

  const stop = useCallback(() => {
    abortRef.current?.abort();
  }, []);

  const send = useCallback(
    async (text: string): Promise<boolean> => {
      const trimmed = text.trim();
      if (!trimmed || streamingRef.current) return false;

      const history: ChatMessage[] = [
        ...messages,
        { role: Role.USER, content: trimmed },
      ];
      // Fixed at send time so the delta loop keeps writing to this message
      // even if another send() appends its own placeholder first.
      const idx = history.length;
      setMessages([...history, { role: Role.ASSISTANT, content: '' }]);
      streamingRef.current = true;
      setStreaming(true);
      setError(null);

      const ac = new AbortController();
      abortRef.current = ac;
      let produced = false;

      try {
        const stream = chatClient.chat(
          { messages: trimHistory(history) },
          { signal: ac.signal }
        );

        for await (const chunk of stream) {
          if (chunk.event.case !== 'textDelta') continue;
          const delta = chunk.event.value;
          if (!delta) continue;
          produced = true;
          setMessages((prev) => {
            const next = [...prev];
            const last = next[idx];
            next[idx] = { ...last, content: last.content + delta };
            return next;
          });
        }
      } catch (err) {
        // An aborted stream is the user pressing Stop, not a failure.
        if (!ac.signal.aborted) {
          setError(err instanceof Error ? err.message : 'Unknown error');
        }
      } finally {
        // Drop the user message too: a blank placeholder would render as an
        // empty bubble and replay as an empty turn in every later request.
        if (!produced) setMessages((prev) => prev.slice(0, idx - 1));
        streamingRef.current = false;
        setStreaming(false);
        abortRef.current = null;
      }

      return produced;
    },
    [messages]
  );

  return { messages, streaming, error, send, stop };
}

export default useChatStream;
