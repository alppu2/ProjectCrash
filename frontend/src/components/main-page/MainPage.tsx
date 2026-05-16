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
