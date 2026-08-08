import { useState } from 'react';
import useChatStream from './useChatStream';
import { Role } from '../../gen/chat_pb';
import './ChatPanel.css';

function ChatPanel() {
  const { messages, streaming, error, send, stop } = useChatStream();
  const [input, setInput] = useState('');

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    const text = input;
    setInput('');
    // send() rolls the turn back when it produced nothing, so hand the text
    // back rather than losing it.
    if (!(await send(text))) setInput(text);
  }

  return (
    <section className="chat-panel">
      <h1>Chat</h1>

      <ol className="chat-messages">
        {messages.map((m, i) => (
          <li
            key={i}
            className={m.role === Role.USER ? 'chat-user' : 'chat-assistant'}
          >
            {m.content}
            {streaming && i === messages.length - 1 && (
              <span className="chat-cursor">▌</span>
            )}
          </li>
        ))}
      </ol>

      {error && <p className="chat-error">{error}</p>}

      <form className="chat-form" onSubmit={handleSubmit}>
        <input
          type="text"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          placeholder="Say something"
          disabled={streaming}
        />
        {streaming ? (
          <button type="button" onClick={stop}>
            Stop
          </button>
        ) : (
          <button type="submit" disabled={!input.trim()}>
            Send
          </button>
        )}
      </form>
    </section>
  );
}

export default ChatPanel;
