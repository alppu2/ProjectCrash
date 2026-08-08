import { useEffect, useRef, useState } from 'react';
import useChatStream from './useChatStream';
import { Role } from '../../gen/chat_pb';
import './ChatPanel.css';

// How close to the bottom still counts as "following along". Fractional
// scroll heights mean an exact comparison never holds.
const PIN_SLACK_PX = 24;

function ChatPanel() {
  const { messages, streaming, error, send, stop } = useChatStream();
  const [input, setInput] = useState('');
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLOListElement>(null);
  const pinnedRef = useRef(true);
  const wasStreamingRef = useRef(false);

  // Follow new deltas, unless the user has scrolled up to read back.
  useEffect(() => {
    const el = listRef.current;
    if (el && pinnedRef.current) el.scrollTop = el.scrollHeight;
  }, [messages]);

  // Clicking Stop unmounts the button that has focus, dropping it to the
  // body. Only refocus on the streaming -> idle edge, so the panel does not
  // steal focus from the rest of the page on mount.
  useEffect(() => {
    if (wasStreamingRef.current && !streaming) inputRef.current?.focus();
    wasStreamingRef.current = streaming;
  }, [streaming]);

  function handleScroll() {
    const el = listRef.current;
    if (!el) return;
    const distance = el.scrollHeight - el.scrollTop - el.clientHeight;
    pinnedRef.current = distance < PIN_SLACK_PX;
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    // The field stays enabled during a reply, so Enter can still reach this.
    // send() would reject it anyway; returning early avoids clearing the
    // field and putting the text straight back.
    if (streaming) return;
    const text = input;
    setInput('');
    // send() rolls the turn back when it produced nothing, so hand the text
    // back rather than losing it.
    if (!(await send(text))) setInput(text);
  }

  return (
    <section className="chat-panel">
      <h1>Chat</h1>

      <ol className="chat-messages" ref={listRef} onScroll={handleScroll}>
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
        {/* Deliberately not disabled while streaming: disabling blurs the
            field, and typing the next message during a reply is useful. */}
        <input
          ref={inputRef}
          type="text"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          placeholder="Say something"
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
