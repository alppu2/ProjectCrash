import { useCallback, useEffect, useRef, useState } from 'react';
import { chatClient } from '../../api';
import { Role } from '../../gen/chat_pb';

export interface ChatMessage {
  role: Role;
  content: string;
}

// The server is stateless, so the client owns the history. Cap it so a long
// conversation does not grow the request without bound.
const MAX_HISTORY = 20;

// The window has to begin on a user turn, so snapping forward can return
// MAX_HISTORY - 1 messages.
function trimHistory(history: ChatMessage[]): ChatMessage[] {
  if (history.length <= MAX_HISTORY) return history;
  let start = history.length - MAX_HISTORY;
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
  // Backs the concurrency guard below. A ref (rather than the `streaming`
  // state value) avoids a stale closure: `streaming` is only current as of
  // the render that created this callback, so two overlapping calls to
  // send() could both read streaming === false before either commits its
  // setStreaming(true).
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
      // Fixed at send time so the delta loop below always writes to this
      // message, even if another send() starts (and appends its own
      // placeholder) before this stream finishes.
      const idx = history.length;
      // Append an empty assistant message that the deltas accumulate into.
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
        // Drop the user message along with its placeholder: an empty assistant
        // message renders as a blank bubble and is replayed as an empty turn in
        // every later request.
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
