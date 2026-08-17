import { beforeEach, describe, expect, it, vi } from 'vitest';
import { act, renderHook } from '@testing-library/react';
import useChatStream, {
  MAX_HISTORY,
  MAX_HISTORY_BYTES,
  trimHistory,
  type ChatMessage,
} from './useChatStream';
import { ChatChunk, Role } from '../../gen/chat_pb';
import { chatClient } from '../../api';

// The transport is the one thing a unit test cannot have: api.ts points at
// Envoy on :8080. Only chat() is stubbed, so everything the hook does with the
// stream — accumulation, rollback, history assembly — is the real code.
vi.mock('../../api', () => ({
  chatClient: { chat: vi.fn() },
}));

const mockChat = vi.mocked(chatClient.chat);

// Mirrors chat.go's maxHistoryBytes. The client cap is deliberately lower;
// this is the wall the server actually rejects at.
const SERVER_MAX_HISTORY_BYTES = 32768;

function user(content: string): ChatMessage {
  return { role: Role.USER, content };
}

function assistant(content: string): ChatMessage {
  return { role: Role.ASSISTANT, content };
}

// A conversation of `turns` exchanges, each assistant reply `replyBytes` long —
// the shape a real model produces, and the one an echo stub never could.
function conversation(turns: number, replyBytes: number): ChatMessage[] {
  const history: ChatMessage[] = [];
  for (let i = 0; i < turns; i += 1) {
    history.push(user(`question ${i}`));
    history.push(assistant('x'.repeat(replyBytes)));
  }
  return history;
}

// Takes anything with a content field, so it measures both ChatMessage[] and
// the PartialMessage<Message>[] that reaches the transport.
function totalBytes(history: readonly { content?: string }[]): number {
  return history.reduce(
    (total, m) => total + new TextEncoder().encode(m.content ?? '').length,
    0
  );
}

// One text_delta frame, as the real client yields it.
function deltaChunk(text: string): ChatChunk {
  return new ChatChunk({ event: { case: 'textDelta', value: text } });
}

describe('trimHistory', () => {
  it('leaves a conversation that fits within both caps untouched', () => {
    const history = [user('hi'), assistant('hello'), user('again')];

    expect(trimHistory(history)).toEqual(history);
  });

  it('drops the oldest turns once the message count exceeds the cap', () => {
    const history = [...conversation(MAX_HISTORY, 5), user('newest')];

    const trimmed = trimHistory(history);

    expect(trimmed.length).toBeLessThanOrEqual(MAX_HISTORY);
    expect(trimmed[trimmed.length - 1]).toEqual(user('newest'));
  });

  it('starts the window on a user turn so the model never opens on a reply', () => {
    // An even-length window would begin on an assistant message, so the snap
    // forward has to drop one more.
    const history = [...conversation(MAX_HISTORY, 5), user('newest')];

    expect(trimHistory(history)[0].role).toBe(Role.USER);
  });

  // The regression this suite exists for: with a count-only cap, a real
  // model's replies push a 20-message window past the server's byte limit,
  // and every later send fails InvalidArgument for the rest of the session.
  it('keeps a long conversation under the server byte limit', () => {
    // 4KB replies, so the count-capped window of MAX_HISTORY messages still
    // carries ~40KB — past what the server accepts.
    const history = [...conversation(12, 4000), user('one more question')];

    const trimmed = trimHistory(history);

    expect(totalBytes(history.slice(-MAX_HISTORY))).toBeGreaterThan(
      SERVER_MAX_HISTORY_BYTES
    );
    expect(totalBytes(trimmed)).toBeLessThanOrEqual(MAX_HISTORY_BYTES);
    expect(trimmed[trimmed.length - 1]).toEqual(user('one more question'));
  });

  it('measures content in UTF-8 bytes, as the server does', () => {
    // Four bytes each in UTF-8, two UTF-16 code units each: counting
    // JavaScript string length would undercount by half and let an oversized
    // history through.
    const emoji = '😀'.repeat(MAX_HISTORY_BYTES / 4);
    const history = [user(emoji), assistant(emoji), user('short')];

    const trimmed = trimHistory(history);

    expect(totalBytes(trimmed)).toBeLessThanOrEqual(MAX_HISTORY_BYTES);
    expect(trimmed[trimmed.length - 1]).toEqual(user('short'));
  });

  it('keeps the message being sent even when it alone exceeds the cap', () => {
    // Trimming cannot save this one — the server rejects it and the UI shows
    // the error. Dropping it here would send an empty history instead, which
    // is a worse error.
    const oversized = user('x'.repeat(MAX_HISTORY_BYTES + 1));
    const history = [user('hi'), assistant('hello'), oversized];

    expect(trimHistory(history)).toEqual([oversized]);
  });
});

describe('useChatStream', () => {
  beforeEach(() => {
    mockChat.mockReset();
  });

  // The wedge as a user meets it: ten-odd exchanges with a real model, and
  // then every further message fails. The rollback in send() restores the same
  // oversized history, so the failure repeats until the page is reloaded.
  it('keeps sending successfully through a long conversation', async () => {
    const requests: { content?: string }[][] = [];
    mockChat.mockImplementation((req) => {
      const messages = req.messages ?? [];
      requests.push(messages);
      if (totalBytes(messages) > SERVER_MAX_HISTORY_BYTES) {
        // What chat.go returns; the hook surfaces it as an error and rolls the
        // turn back.
        throw new Error('history content exceeds the limit of 32768 bytes');
      }
      return (async function* () {
        yield deltaChunk('x'.repeat(4000));
      })();
    });

    const { result } = renderHook(() => useChatStream());
    for (let i = 0; i < 12; i += 1) {
      await act(async () => {
        await result.current.send(`question ${i}`);
      });
    }

    expect(result.current.error).toBeNull();
    expect(requests).toHaveLength(12);
    for (const messages of requests) {
      expect(totalBytes(messages)).toBeLessThanOrEqual(MAX_HISTORY_BYTES);
    }
    // Every turn produced a reply, so nothing was rolled back: 12 questions
    // and 12 answers.
    expect(result.current.messages).toHaveLength(24);
  });

  it('rolls the failed turn back so the next send is not poisoned', async () => {
    mockChat.mockImplementationOnce(() => {
      throw new Error('llm provider unreachable');
    });
    mockChat.mockImplementation(() =>
      (async function* () {
        yield deltaChunk('hello');
      })()
    );

    const { result } = renderHook(() => useChatStream());
    await act(async () => {
      await result.current.send('first');
    });

    expect(result.current.error).toBe('llm provider unreachable');
    expect(result.current.messages).toHaveLength(0);

    await act(async () => {
      await result.current.send('second');
    });

    expect(result.current.error).toBeNull();
    expect(result.current.messages).toEqual([
      { role: Role.USER, content: 'second' },
      { role: Role.ASSISTANT, content: 'hello' },
    ]);
  });
});
