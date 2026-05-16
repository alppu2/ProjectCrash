import { useCallback, useEffect, useRef, useState } from 'react';
import { client } from '../../client';

interface StreamResult {
  message: string;
  success: boolean;
}

function useStreamDisturbance(clientId: string, payload: string, packetCount: number) {
  const [progress, setProgress] = useState(0);
  const [streaming, setStreaming] = useState(false);
  const [result, setResult] = useState<StreamResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const startStream = useCallback(async () => {
    if (streaming) return;

    setStreaming(true);
    setProgress(0);
    setResult(null);
    setError(null);

    try {
      const res = await client.streamDisturbance(async function* () {
        for (let i = 0; i < packetCount; i++) {
          yield { clientId, payload, sequenceNumber: i };
          if (mountedRef.current) setProgress(i + 1);
        }
      });
      if (mountedRef.current) setResult({ message: res.message, success: res.success });
    } catch (err) {
      if (mountedRef.current) setError(err instanceof Error ? err.message : 'Unknown error');
    } finally {
      if (mountedRef.current) setStreaming(false);
    }
  }, [clientId, payload, packetCount, streaming]);

  return { progress, streaming, result, error, startStream };
}

export default useStreamDisturbance;
