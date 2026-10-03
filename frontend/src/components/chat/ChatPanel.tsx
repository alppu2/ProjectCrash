import { useEffect, useRef, useState } from 'react';
import useChatStream from './useChatStream';
import useWarmup, { type WarmupStatus } from './useWarmup';
import { Role } from '../../gen/chat_pb';
import { ArrowUpIcon, StopIcon } from '../icons';

const BUBBLE = 'max-w-[85%] whitespace-pre-wrap rounded-2xl px-4 py-2.5';
const ASSISTANT_BUBBLE = `${BUBBLE} self-start rounded-bl-sm border border-border bg-surface`;
const USER_BUBBLE = `${BUBBLE} self-end rounded-br-sm bg-fg text-bg`;
const BANNER = 'rounded-lg bg-danger-bg px-3 py-2 text-sm text-danger';
const ICON_BUTTON =
  'grid size-9 shrink-0 place-items-center rounded-xl bg-accent text-accent-fg transition-opacity hover:opacity-85 disabled:cursor-not-allowed disabled:opacity-40 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent';

// How close to the bottom still counts as "following along". Fractional
// scroll heights mean an exact comparison never holds.
const PIN_SLACK_PX = 24;

// How long the scrollbar stays drawn after the last scroll event.
const SCROLLBAR_LINGER_MS = 1000;

const PLACEHOLDER: Record<WarmupStatus, string> = {
  warming: 'Assistant is warming up…',
  ready: 'Say something',
  unavailable: 'Assistant is offline',
};

function ChatPanel() {
  const { messages, streaming, error, send, stop } = useChatStream();
  const { status, welcome, retry } = useWarmup();
  const [input, setInput] = useState('');
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLOListElement>(null);
  const pinnedRef = useRef(true);
  const wasStreamingRef = useRef(false);
  const retriedRef = useRef(false);
  const scrollIdleRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => () => clearTimeout(scrollIdleRef.current), []);

  // Follow new deltas, unless the user has scrolled up to read back.
  useEffect(() => {
    const el = listRef.current;
    if (el && pinnedRef.current) el.scrollTop = el.scrollHeight;
  }, [messages, welcome]);

  // Clicking Stop unmounts the button that has focus, dropping it to the
  // body. Only refocus on the streaming -> idle edge, so the panel does not
  // steal focus from the rest of the page on mount.
  useEffect(() => {
    if (wasStreamingRef.current && !streaming) inputRef.current?.focus();
    wasStreamingRef.current = streaming;
  }, [streaming]);

  // Retry unmounts its own focused button the same way; refocus only after a
  // retry, never on the first warmup.
  useEffect(() => {
    if (status === 'ready' && retriedRef.current) {
      retriedRef.current = false;
      inputRef.current?.focus();
    }
  }, [status]);

  function handleRetry() {
    retriedRef.current = true;
    retry();
  }

  function handleScroll() {
    const el = listRef.current;
    if (!el) return;
    // On the node, not in state: scroll fires per frame, and only CSS reads it.
    el.setAttribute('data-scrolling', '');
    clearTimeout(scrollIdleRef.current);
    scrollIdleRef.current = setTimeout(
      () => el.removeAttribute('data-scrolling'),
      SCROLLBAR_LINGER_MS
    );
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
    <section aria-label="Chat" className="flex min-h-0 flex-1 flex-col">
      <ol
        ref={listRef}
        onScroll={handleScroll}
        className="scrollbar-autohide flex flex-1 flex-col gap-3 overflow-y-auto py-6 text-[15px] leading-relaxed"
      >
        {status === 'warming' && (
          <li className="flex items-center gap-2 self-start text-sm text-muted">
            <span
              aria-hidden="true"
              className="size-2 animate-pulse rounded-full bg-accent"
            />
            <span role="status">{PLACEHOLDER.warming}</span>
          </li>
        )}
        {welcome && <li className={ASSISTANT_BUBBLE}>{welcome}</li>}
        {messages.map((m, i) => {
          const live = streaming && i === messages.length - 1;
          if (live && !m.content) {
            return (
              <li key={i} className={ASSISTANT_BUBBLE}>
                <span role="status" className="sr-only">
                  Assistant is typing
                </span>
                <span
                  aria-hidden="true"
                  className="flex h-6 items-center gap-1"
                >
                  {[0, 150, 300].map((delay) => (
                    <span
                      key={delay}
                      style={{ animationDelay: `${delay}ms` }}
                      className="size-1.5 animate-typing rounded-full bg-muted motion-reduce:animate-none"
                    />
                  ))}
                </span>
              </li>
            );
          }
          return (
            <li
              key={i}
              className={m.role === Role.USER ? USER_BUBBLE : ASSISTANT_BUBBLE}
            >
              {m.content}
              {live && (
                <span
                  aria-hidden="true"
                  className="ml-0.5 inline-block h-[1.1em] w-0.5 translate-y-[0.2em] animate-pulse bg-current opacity-60"
                />
              )}
            </li>
          );
        })}
      </ol>

      <div className="flex flex-col gap-2 pb-4">
        {status === 'unavailable' && (
          <p role="alert" className={BANNER}>
            The assistant is offline right now.{' '}
            <button
              type="button"
              onClick={handleRetry}
              className="font-medium underline underline-offset-2 hover:no-underline"
            >
              Retry
            </button>
          </p>
        )}
        {error && <p className={BANNER}>{error}</p>}

        <form
          onSubmit={handleSubmit}
          className="flex items-center gap-2 rounded-2xl border border-border bg-surface p-1.5 pl-4 transition-[border-color] focus-within:border-accent"
        >
          {/* Deliberately not disabled while streaming: disabling blurs the
              field, and typing the next message during a reply is useful. */}
          <input
            ref={inputRef}
            type="text"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder={PLACEHOLDER[status]}
            disabled={status !== 'ready'}
            className="min-w-0 flex-1 bg-transparent py-2 outline-none placeholder:text-muted disabled:cursor-not-allowed"
          />
          {streaming ? (
            <button
              type="button"
              onClick={stop}
              aria-label="Stop"
              title="Stop"
              className={ICON_BUTTON}
            >
              <StopIcon className="size-4" />
            </button>
          ) : (
            <button
              type="submit"
              disabled={status !== 'ready' || !input.trim()}
              aria-label="Send"
              title="Send"
              className={ICON_BUTTON}
            >
              <ArrowUpIcon className="size-4" />
            </button>
          )}
        </form>
      </div>
    </section>
  );
}

export default ChatPanel;
