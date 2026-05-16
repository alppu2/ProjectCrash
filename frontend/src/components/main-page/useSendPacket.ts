import { useState } from 'react';
import { client } from '../../client';

interface FormState {
  clientId: string;
  payload: string;
  sequenceNumber: string;
}

interface ResponseState {
  message: string;
  success: boolean;
}

function useSendPacket() {
  const [form, setForm] = useState<FormState>({
    clientId: '',
    payload: '',
    sequenceNumber: '0',
  });
  const [response, setResponse] = useState<ResponseState | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  function handleChange(e: React.ChangeEvent<HTMLInputElement>) {
    setForm((prev) => ({ ...prev, [e.target.name]: e.target.value }));
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setLoading(true);
    setError(null);
    setResponse(null);

    try {
      const res = await client.sendPacket({
        clientId: form.clientId,
        payload: form.payload,
        sequenceNumber: parseInt(form.sequenceNumber, 10),
      });
      setResponse({ message: res.message, success: res.success });
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unknown error');
    } finally {
      setLoading(false);
    }
  }

  return { form, response, loading, error, handleChange, handleSubmit };
}

export default useSendPacket;
