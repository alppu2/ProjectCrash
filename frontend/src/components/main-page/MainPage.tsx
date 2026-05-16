import useSendPacket from './useSendPacket';
import useStreamDisturbance from './useStreamDisturbance';
import './MainPage.css';

function MainPage() {
  const { form, response, loading, error, handleChange, handleSubmit } =
    useSendPacket();

  const packetCount = parseInt(form.packetCount, 10) || 1;

  const {
    progress,
    streaming,
    result: streamResult,
    error: streamError,
    startStream,
  } = useStreamDisturbance(form.clientId, form.payload, packetCount);

  const isStreaming = packetCount > 1;
  const busy = loading || streaming;

  function handleSend(e: React.FormEvent) {
    if (isStreaming) {
      e.preventDefault();
      startStream();
    } else {
      handleSubmit(e);
    }
  }

  const activeResult = isStreaming ? streamResult : response;
  const activeError = isStreaming ? streamError : error;

  return (
    <section id="center">
      <h1>Send Packet</h1>
      <form className="packet-form" onSubmit={handleSend}>
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
        <label>
          Packet Count
          <input
            name="packetCount"
            type="number"
            value={form.packetCount}
            onChange={handleChange}
            min={1}
            required
          />
        </label>
        <button type="submit" disabled={busy}>
          {busy
            ? isStreaming
              ? 'Streaming…'
              : 'Sending…'
            : `Send${packetCount > 1 ? ` ${packetCount} Packets` : ' Packet'}`}
        </button>
      </form>

      {isStreaming && (streaming || streamResult || streamError) && (
        <p className="progress">Sent {progress} / {packetCount}</p>
      )}

      {activeResult && (
        <div className={`response ${activeResult.success ? 'success' : 'failure'}`}>
          <strong>{activeResult.success ? 'Success' : 'Failed'}</strong>
          <span>{activeResult.message}</span>
        </div>
      )}

      {activeError && (
        <div className="response failure">
          <strong>Error</strong>
          <span>{activeError}</span>
        </div>
      )}
    </section>
  );
}

export default MainPage;
