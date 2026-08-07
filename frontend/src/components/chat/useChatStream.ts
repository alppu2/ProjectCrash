import { useCallback, useEffect, useRef, useState } from 'react';
import { chatClient } from '../../chatClient';
import { Role } from '../../gen/chat_pb';

export interface ChatMessage {
  role: Role;
  content: string;
}

// The server is stateless, so the client owns the history. Cap it so a long
// conversation does not grow the request without bound.
const MAX_HISTORY = 20;

function useChatStream() {
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [streaming, setStreaming] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => () => abortRef.current?.abort(), []);

  const stop = useCallback(() => {
    abortRef.current?.abort();
  }, []);

  const send = useCallback(
    async (text: string) => {
      const trimmed = text.trim();
      if (!trimmed || streaming) return;

      const history: ChatMessage[] = [...messages, { role: Role.USER, content: trimmed }];
      // Append an empty assistant message that the deltas accumulate into.
      setMessages([...history, { role: Role.ASSISTANT, content: '' }]);
      setStreaming(true);
      setError(null);

      const ac = new AbortController();
      abortRef.current = ac;

      try {
        const stream = chatClient.chat(
          { messages: history.slice(-MAX_HISTORY) },
          { signal: ac.signal }
        );

        for await (const chunk of stream) {
          if (chunk.event.case !== 'textDelta') continue;
          const delta = chunk.event.value;
          setMessages((prev) => {
            const next = [...prev];
            const last = next[next.length - 1];
            next[next.length - 1] = { ...last, content: last.content + delta };
            return next;
          });
        }
      } catch (err) {
        // An aborted stream is the user pressing Stop, not a failure.
        if (!ac.signal.aborted) {
          setError(err instanceof Error ? err.message : 'Unknown error');
        }
      } finally {
        setStreaming(false);
        abortRef.current = null;
      }
    },
    [messages, streaming]
  );

  return { messages, streaming, error, send, stop };
}

export default useChatStream;
