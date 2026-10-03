import { useCallback, useEffect, useState } from 'react';
import { chatClient } from '../../api';
import { WELCOME_MESSAGE } from './welcome';

export type WarmupStatus = 'warming' | 'ready' | 'unavailable';

// Above the server's 120s warm timeout, so a slow cold load fails there with a
// logged reason rather than as a bare client deadline.
export const WARMUP_TIMEOUT_MS = 130_000;

// Paces the welcome like a streamed reply.
const WORD_DELAY_MS = 40;

function useWarmup(wordDelayMs = WORD_DELAY_MS) {
  const [status, setStatus] = useState<WarmupStatus>('warming');
  const [attempt, setAttempt] = useState(0);
  const [welcome, setWelcome] = useState('');

  useEffect(() => {
    const ac = new AbortController();
    chatClient
      .warmup({}, { signal: ac.signal, timeoutMs: WARMUP_TIMEOUT_MS })
      .then(() => setStatus('ready'))
      .catch(() => {
        // StrictMode's discarded mount aborts its call; only the live one reports.
        if (!ac.signal.aborted) setStatus('unavailable');
      });
    return () => ac.abort();
  }, [attempt]);

  useEffect(() => {
    if (status !== 'ready') return;
    const words = WELCOME_MESSAGE.split(' ');
    let shown = 0;
    const id = setInterval(() => {
      shown += 1;
      setWelcome(words.slice(0, shown).join(' '));
      if (shown >= words.length) clearInterval(id);
    }, wordDelayMs);
    return () => clearInterval(id);
  }, [status, wordDelayMs]);

  const retry = useCallback(() => {
    setStatus('warming');
    setAttempt((n) => n + 1);
  }, []);

  return { status, welcome, retry };
}

export default useWarmup;
