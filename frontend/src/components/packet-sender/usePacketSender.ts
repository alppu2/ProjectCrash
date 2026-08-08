import { useCallback, useEffect, useRef, useState } from 'react';
import { client } from '../../api';

interface FormState {
  clientId: string;
  payload: string;
  sequenceNumber: string;
  packetCount: string;
}

interface Result {
  message: string;
  success: boolean;
}

function usePacketSender() {
  const [form, setForm] = useState<FormState>({
    clientId: '',
    payload: '',
    sequenceNumber: '0',
    packetCount: '1',
  });
  const [result, setResult] = useState<Result | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const packetCount = parseInt(form.packetCount, 10) || 1;
  const isStreaming = packetCount > 1;

  function handleChange(e: React.ChangeEvent<HTMLInputElement>) {
    setForm((prev) => ({ ...prev, [e.target.name]: e.target.value }));
  }

  const handleSend = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      if (busy) return;

      setBusy(true);
      setResult(null);
      setError(null);

      try {
        if (packetCount === 1) {
          const res = await client.sendPacket({
            clientId: form.clientId,
            payload: form.payload,
            sequenceNumber: parseInt(form.sequenceNumber, 10),
          });
          if (mountedRef.current)
            setResult({ message: res.message, success: res.success });
        } else {
          const res = await client.stressTest({
            clientId: form.clientId,
            payload: form.payload,
            count: packetCount,
          });
          if (mountedRef.current)
            setResult({ message: res.message, success: res.success });
        }
      } catch (err) {
        if (mountedRef.current)
          setError(err instanceof Error ? err.message : 'Unknown error');
      } finally {
        if (mountedRef.current) setBusy(false);
      }
    },
    [form, packetCount, busy]
  );

  return {
    form,
    handleChange,
    handleSend,
    busy,
    result,
    error,
    isStreaming,
    packetCount,
  };
}

export default usePacketSender;
