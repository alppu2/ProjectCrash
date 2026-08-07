import usePacketSender from './usePacketSender';
import './PacketSender.css';

function PacketSender() {
  const { form, handleChange, handleSend, busy, result, error, isStreaming, packetCount } =
    usePacketSender();

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

      {result && (
        <div className={`response ${result.success ? 'success' : 'failure'}`}>
          <strong>{result.success ? 'Success' : 'Failed'}</strong>
          <span>{result.message}</span>
        </div>
      )}

      {error && (
        <div className="response failure">
          <strong>Error</strong>
          <span>{error}</span>
        </div>
      )}
    </section>
  );
}

export default PacketSender;
